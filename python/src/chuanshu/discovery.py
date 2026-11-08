"""Zero-config LAN peer discovery over IPv4 multicast.

No tracker: every peer periodically broadcasts the files it offers (info hash,
TCP port, completeness) to a well-known multicast group and also replies
directly to any announcer, so newcomers learn the whole swarm within seconds.

Wire format (identical to the Go implementation):

    [4] magic "CHU1" | [1] type (1=announce, 2=reply) | [1] count
    then per advert: [32] info hash | [2] tcp port | [1] flags (bit0=complete)
"""
from __future__ import annotations

import errno
import socket
import struct
import threading
import time
from typing import Dict, List, Optional, Set, Tuple

DISCOVERY_UDP_PORT = 51218
MAGIC = 0x43485531  # "CHU1"
MULTICAST_GROUP = "239.77.84.1"

TYPE_ANNOUNCE = 1
TYPE_REPLY = 2
FLAG_COMPLETE = 1 << 0

_ANNOUNCE_INTERVAL = 3.0
_PRUNE_INTERVAL = 5.0
_MAX_AGE = 15.0


class Advert:
    __slots__ = ("info_hash", "complete")

    def __init__(self, info_hash: bytes, complete: bool) -> None:
        self.info_hash = info_hash
        self.complete = complete


class Peer:
    __slots__ = ("tcp_addr", "info_hash", "complete", "last_seen")

    def __init__(self, tcp_addr: str, info_hash: bytes, complete: bool,
                 last_seen: float) -> None:
        self.tcp_addr = tcp_addr
        self.info_hash = info_hash
        self.complete = complete
        self.last_seen = last_seen


class Registry:
    """Peer registry keyed by info-hash hex, then peer address. Thread-safe."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._peers: Dict[str, Dict[str, Peer]] = {}

    def add(self, info_hash_hex: str, peer: Peer) -> None:
        with self._lock:
            m = self._peers.setdefault(info_hash_hex, {})
            existing = m.get(peer.tcp_addr)
            if existing is None:
                m[peer.tcp_addr] = peer
            else:
                # Sticky-complete: once a peer has been seen complete, keep it.
                existing.last_seen = peer.last_seen
                existing.complete = existing.complete or peer.complete

    def peers(self, info_hash_hex: str, self_addr: str,
              max_age: float = _MAX_AGE) -> List[Peer]:
        cutoff = time.time() - max_age
        out: List[Peer] = []
        with self._lock:
            for p in self._peers.get(info_hash_hex, {}).values():
                if p.last_seen < cutoff or p.tcp_addr == self_addr:
                    continue
                out.append(p)
        return out

    def prune(self, max_age: float = _MAX_AGE) -> None:
        cutoff = time.time() - max_age
        with self._lock:
            for h in list(self._peers):
                m = self._peers[h]
                for addr, p in list(m.items()):
                    if p.last_seen < cutoff:
                        del m[addr]
                if not m:
                    del self._peers[h]

    def hashes(self, max_age: float = _MAX_AGE) -> List[str]:
        cutoff = time.time() - max_age
        out: List[str] = []
        with self._lock:
            for h, m in self._peers.items():
                if any(p.last_seen >= cutoff for p in m.values()):
                    out.append(h)
        return out

    def seeders(self, info_hash_hex: str, max_age: float = _MAX_AGE) -> int:
        cutoff = time.time() - max_age
        with self._lock:
            return sum(1 for p in self._peers.get(info_hash_hex, {}).values()
                       if p.last_seen >= cutoff and p.complete)


def encode_packet(pkt_type: int, tcp_port: int,
                  adverts: List[Advert]) -> Optional[bytes]:
    if not adverts or len(adverts) > 255:
        return None
    parts = [struct.pack(">IBB", MAGIC, pkt_type, len(adverts))]
    for a in adverts:
        flags = FLAG_COMPLETE if a.complete else 0
        parts.append(a.info_hash + struct.pack(">HB", tcp_port, flags))
    return b"".join(parts)


def decode_packet(pkt: bytes) -> Tuple[int, List[Tuple[bytes, int, bool]]]:
    """Return (type, [(info_hash, tcp_port, complete), ...]).

    Raises ValueError on malformed packets.
    """
    if len(pkt) < 6:
        raise ValueError("too short")
    (magic,) = struct.unpack(">I", pkt[:4])
    if magic != MAGIC:
        raise ValueError("bad magic")
    pkt_type = pkt[4]
    count = pkt[5]
    if len(pkt) < 6 + count * 35:
        raise ValueError("truncated")
    entries = []
    off = 6
    for _ in range(count):
        info_hash = pkt[off:off + 32]
        (port,) = struct.unpack(">H", pkt[off + 32:off + 34])
        flags = pkt[off + 34]
        entries.append((info_hash, port, bool(flags & FLAG_COMPLETE)))
        off += 35
    return pkt_type, entries


def _make_socket(port: int) -> socket.socket:
    """Create a multicast-capable UDP socket with best-effort options.

    SO_REUSEPORT lets several peers share the discovery port on one host
    (macOS/BSD); Windows uses SO_REUSEADDR; Solaris and others simply get
    whatever the platform allows. Joining the group happens in start().
    """
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        if hasattr(socket, "SO_REUSEPORT"):
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEPORT, 1)
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    except OSError:
        pass
    sock.bind(("", port))
    try:
        mreq = socket.inet_aton(MULTICAST_GROUP) + socket.inet_aton("0.0.0.0")
        sock.setsockopt(socket.IPPROTO_IP, socket.IP_ADD_MEMBERSHIP, mreq)
    except OSError as e:
        sock.close()
        raise OSError(errno.EADDRINUSE if e.errno == errno.EADDRINUSE else e.errno,
                      f"join multicast {MULTICAST_GROUP}: {e}") from e
    # Deliver our own packets so multiple instances on one host discover each
    # other (matches the Go node behaviour).
    try:
        sock.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_TTL, 255)
        sock.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_LOOP, 1)
    except OSError:
        pass
    return sock


class Service:
    """UDP multicast announcer/listener. Mirrors discovery.Service in Go."""

    def __init__(self, udp_port: int = DISCOVERY_UDP_PORT) -> None:
        self.registry = Registry()
        self._udp_port = udp_port
        self._tcp_port = 0
        self._lock = threading.Lock()
        self._adverts: List[Advert] = []
        self._sock: Optional[socket.socket] = None
        self._stop = threading.Event()
        self._threads: List[threading.Thread] = []

    def set_data_port(self, port: int) -> None:
        with self._lock:
            self._tcp_port = port

    def set_advertised(self, adverts: List[Advert]) -> None:
        with self._lock:
            self._adverts = list(adverts)

    def _snapshot(self) -> Tuple[int, List[Advert]]:
        with self._lock:
            return self._tcp_port, list(self._adverts)

    def start(self) -> None:
        self._sock = _make_socket(self._udp_port)
        for target in (self._read_loop, self._announce_loop, self._prune_loop):
            t = threading.Thread(target=target, daemon=True)
            t.start()
            self._threads.append(t)

    def stop(self) -> None:
        self._stop.set()
        if self._sock is not None:
            try:
                self._sock.close()
            except OSError:
                pass
        for t in self._threads:
            t.join(timeout=3)

    # ---- loops ----

    def _broadcast_all(self) -> None:
        tcp_port, adverts = self._snapshot()
        if not adverts or self._sock is None:
            return
        pkt = encode_packet(TYPE_ANNOUNCE, tcp_port, adverts)
        if pkt is None:
            return
        try:
            self._sock.sendto(pkt, (MULTICAST_GROUP, self._udp_port))
        except OSError:
            pass

    def _announce_loop(self) -> None:
        # Announce once immediately, then on an interval.
        self._broadcast_all()
        while not self._stop.wait(_ANNOUNCE_INTERVAL):
            self._broadcast_all()

    def _prune_loop(self) -> None:
        while not self._stop.wait(_PRUNE_INTERVAL):
            self.registry.prune(_MAX_AGE)

    def _read_loop(self) -> None:
        assert self._sock is not None
        self._sock.settimeout(1.0)
        while not self._stop.is_set():
            try:
                pkt, (addr, _) = self._sock.recvfrom(2048)
            except socket.timeout:
                continue
            except OSError:
                return
            try:
                pkt_type, entries = decode_packet(pkt)
            except ValueError:
                continue
            now = time.time()
            for info_hash, port, complete in entries:
                peer = Peer(f"{addr}:{port}", info_hash, complete, now)
                self.registry.add(info_hash.hex(), peer)
            # Reply directly so the announcer learns our set quickly.
            if pkt_type == TYPE_ANNOUNCE:
                tcp_port, adverts = self._snapshot()
                if adverts and self._sock is not None:
                    rp = encode_packet(TYPE_REPLY, tcp_port, adverts)
                    if rp is not None:
                        try:
                            self._sock.sendto(rp, (addr, self._udp_port))
                        except OSError:
                            pass
