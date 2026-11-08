"""The swarm node: TCP piece server + downloader + seeder.

Mirrors internal/swarm/swarm.go: serves pieces over TCP, dials discovered
peers for files it still needs, verifies every piece against its hash, and
keeps seeding pieces it has so every downloader becomes a seeder.
"""
from __future__ import annotations

import json
import logging
import os
import random
import socket
import threading
import time
from dataclasses import dataclass
from typing import Callable, Dict, List, Optional

from . import discovery, protocol, store

MAX_AGE = discovery._MAX_AGE
BATCH = 4


@dataclass
class Status:
    info_hash: str
    name: str
    length: int
    pieces: int
    have_pieces: int
    complete: bool
    progress: float
    peers: int
    downloaded: int
    uploaded: int

    def to_dict(self) -> dict:
        return {
            "infoHash": self.info_hash,
            "name": self.name,
            "length": self.length,
            "pieces": self.pieces,
            "havePieces": self.have_pieces,
            "complete": self.complete,
            "progress": self.progress,
            "peers": self.peers,
            "downloaded": self.downloaded,
            "uploaded": self.uploaded,
        }


class _File:
    """Runtime state for one shared file."""

    def __init__(self, meta: protocol.Metadata, st: store.Store,
                 have: protocol.Bitfield) -> None:
        self.meta = meta
        self.store = st
        self.have = have
        self.lock = threading.Lock()   # guards peers set + counters
        self.peers: set = set()
        self.dl = 0
        self.ul = 0


class Node:
    """A running chuanshu instance: TCP server + discovery + file states."""

    def __init__(self, discovery_port: int = discovery.DISCOVERY_UDP_PORT,
                 data_port: int = 0,
                 on_progress: Optional[Callable[[Status], None]] = None) -> None:
        self._data_port = data_port
        self._peer_id = os.urandom(8)
        self._disc = discovery.Service(discovery_port)
        self.registry = self._disc.registry
        self.on_progress = on_progress

        self._lock = threading.RLock()
        self._files: Dict[str, _File] = {}

        self._tcp_addr = ""
        self._listener: Optional[socket.socket] = None
        self._stop = threading.Event()
        self._log = logging.getLogger("chuanshu")

    # ---- lifecycle ----

    def start(self) -> None:
        self._listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self._listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self._listener.bind(("", self._data_port))
        self._listener.listen(64)
        self._data_port = self._listener.getsockname()[1]
        self._tcp_addr = f"{_local_addr()}:{self._data_port}"
        self._disc.set_data_port(self._data_port)
        self._disc.start()
        for target in (self._accept_loop, self._connect_loop, self._publish_loop):
            threading.Thread(target=target, daemon=True).start()

    def stop(self) -> None:
        self._stop.set()
        if self._listener is not None:
            try:
                self._listener.close()
            except OSError:
                pass
        self._disc.stop()
        with self._lock:
            for f in self._files.values():
                f.store.close()

    def my_addr(self) -> str:
        return self._tcp_addr

    # ---- file registration ----

    def add_seed(self, path: str, piece_len: int = 0) -> protocol.Metadata:
        meta = store.build_metadata(path, piece_len)
        key = meta.info_hash_hex()
        with self._lock:
            if key in self._files:
                return meta
            st = store.Store.open_existing(path, meta)
            have = protocol.Bitfield(meta.num_pieces())
            for i in range(meta.num_pieces()):
                have.set(i)
            self._files[key] = _File(meta, st, have)
        self._refresh_adverts()
        return meta

    def start_download(self, dir_path: str, meta: protocol.Metadata) -> None:
        key = meta.info_hash_hex()
        with self._lock:
            if key in self._files:
                return
            st = store.Store.create_download(dir_path, meta)
            have = protocol.Bitfield(meta.num_pieces())
            for i in range(meta.num_pieces()):
                if st.has_piece(i):
                    have.set(i)
            if have.complete():
                try:
                    st.finalize()
                except OSError:
                    st.delete_partial()
                    raise
            self._files[key] = _File(meta, st, have)
        self._refresh_adverts()

    def abort_download(self, info_hash_hex: str) -> None:
        with self._lock:
            f = self._files.get(info_hash_hex)
            if f is None or f.have.complete():
                return
            f.store.delete_partial()
            del self._files[info_hash_hex]

    # ---- queries ----

    def discovered_hashes(self) -> List[str]:
        return self.registry.hashes(MAX_AGE)

    def seeder_count(self, info_hash_hex: str) -> int:
        return self.registry.seeders(info_hash_hex, MAX_AGE)

    def find_peer(self, info_hash_hex: str) -> str:
        try:
            raw = bytes.fromhex(info_hash_hex)
        except ValueError:
            return ""
        if len(raw) != 32:
            return ""
        peers = self.registry.peers(info_hash_hex, self._tcp_addr, MAX_AGE)
        return peers[0].tcp_addr if peers else ""

    def status_for(self, info_hash_hex: str) -> Optional[Status]:
        with self._lock:
            f = self._files.get(info_hash_hex)
            return self._status_locked(f) if f else None

    def all_status(self) -> List[Status]:
        with self._lock:
            return [self._status_locked(f) for f in self._files.values()]

    def _status_locked(self, f: _File) -> Status:
        total = f.meta.num_pieces()
        have = f.have.count()
        progress = (have / total) if total > 0 else 0.0
        peers = len(self.registry.peers(f.meta.info_hash_hex(), self._tcp_addr, MAX_AGE))
        with f.lock:
            dl, ul = f.dl, f.ul
        return Status(
            info_hash=f.meta.info_hash_hex(),
            name=f.meta.name,
            length=f.meta.length,
            pieces=total,
            have_pieces=have,
            complete=have == total,
            progress=progress,
            peers=peers,
            downloaded=dl,
            uploaded=ul,
        )

    def wait_complete(self, info_hash_hex: str) -> None:
        while not self._stop.is_set():
            with self._lock:
                f = self._files.get(info_hash_hex)
                if f is None or f.have.complete():
                    return
            self._stop.wait(0.2)

    # ---- metadata retrieval ----

    def get_meta(self, addr: str, info_hash: bytes) -> protocol.Metadata:
        sock = self._connect(addr)
        try:
            self._send_hello(sock, info_hash)
            protocol.write_frame(sock, protocol.MSG_GETMETA)
            sock.settimeout(10)
            msg_type, payload = protocol.read_frame(sock, protocol.META_MAX_FRAME)
            if msg_type != protocol.MSG_META:
                raise protocol.ProtocolError("unknown info hash")
            meta = protocol.Metadata.from_json(payload)
            meta.verify()
            if meta.info_hash() != info_hash:
                raise protocol.HashMismatchError("metadata hash mismatch")
            return meta
        finally:
            sock.close()

    # ---- advert publishing ----

    def _refresh_adverts(self) -> None:
        with self._lock:
            adverts = [
                discovery.Advert(bytes.fromhex(h), f.have.complete())
                for h, f in self._files.items()
            ]
        self._disc.set_advertised(adverts)

    def _publish_loop(self) -> None:
        while not self._stop.wait(2.0):
            self._refresh_adverts()

    # ---- dialing peers ----

    def _connect_loop(self) -> None:
        while not self._stop.wait(2.0):
            self._dial_needed_peers()

    def _dial_needed_peers(self) -> None:
        targets = []
        with self._lock:
            items = list(self._files.items())
        for hex_hash, f in items:
            if f.have.complete():
                continue
            for p in self.registry.peers(hex_hash, self._tcp_addr, MAX_AGE):
                with f.lock:
                    if p.tcp_addr in f.peers:
                        continue
                    f.peers.add(p.tcp_addr)
                targets.append((f, p.tcp_addr))
        for f, addr in targets:
            threading.Thread(target=self._work_with_peer, args=(f, addr),
                             daemon=True).start()

    def _work_with_peer(self, f: _File, addr: str) -> None:
        try:
            self._download_from_peer(f, addr)
        except (protocol.ProtocolError, OSError):
            pass
        finally:
            with f.lock:
                f.peers.discard(addr)

    def _download_from_peer(self, f: _File, addr: str) -> None:
        sock = self._connect(addr)
        try:
            self._send_hello(sock, f.meta.info_hash())
            while not self._stop.is_set():
                missing = f.have.missing()
                if not missing:
                    return
                batch = missing[:BATCH]
                for idx in batch:
                    protocol.write_frame(sock, protocol.MSG_REQUEST,
                                         protocol.encode_index(idx))
                got = 0
                sock.settimeout(15)
                while got < len(batch):
                    msg_type, payload = protocol.read_frame(sock)
                    if msg_type == protocol.MSG_PIECE:
                        idx, data = protocol.decode_piece_resp(payload)
                        got += 1
                        if f.have.has(idx):
                            continue  # another peer won the race
                        try:
                            f.store.write_piece(idx, data)
                        except protocol.HashMismatchError:
                            self._log.warning("dropping peer %s: bad piece %d",
                                              addr, idx)
                            return
                        f.have.set(idx)
                        with f.lock:
                            f.dl += len(data)
                        self._notify(f)
                        if f.have.complete():
                            self._finalize_if_complete(f)
                            return
                    elif msg_type == protocol.MSG_REJECT:
                        got += 1
                    else:
                        return
        finally:
            sock.close()

    # ---- TCP server ----

    def _accept_loop(self) -> None:
        assert self._listener is not None
        while not self._stop.is_set():
            try:
                conn, _ = self._listener.accept()
            except OSError:
                return
            threading.Thread(target=self._serve_conn, args=(conn,),
                             daemon=True).start()

    def _serve_conn(self, conn: socket.socket) -> None:
        try:
            conn.settimeout(5)
            msg_type, payload = protocol.read_frame(conn)
            if msg_type != protocol.MSG_HELLO:
                return
            version, _peer_id, info_hash = protocol.decode_hello(payload)
            if version != protocol.VERSION or len(info_hash) != 32:
                return
            hex_hash = info_hash.hex()
            with self._lock:
                f = self._files.get(hex_hash)
            conn.settimeout(30)
            if f is None:
                self._serve_unknown(conn)
            else:
                self._serve_file(conn, f)
        except (protocol.ProtocolError, OSError, ValueError, KeyError):
            pass
        finally:
            conn.close()

    def _serve_unknown(self, conn: socket.socket) -> None:
        """Peer asked about a file we do not have: reject one request."""
        try:
            conn.settimeout(5)
            msg_type, _ = protocol.read_frame(conn)
            if msg_type == protocol.MSG_GETMETA:
                protocol.write_frame(conn, protocol.MSG_REJECT)
        except (protocol.ProtocolError, OSError):
            pass

    def _serve_file(self, conn: socket.socket, f: _File) -> None:
        while not self._stop.is_set():
            msg_type, payload = protocol.read_frame(conn)  # 30s timeout
            if msg_type == protocol.MSG_GETMETA:
                protocol.write_frame(conn, protocol.MSG_META, f.meta.to_json())
            elif msg_type == protocol.MSG_REQUEST:
                idx = protocol.decode_index(payload)
                if not f.have.has(idx):
                    protocol.write_frame(conn, protocol.MSG_REJECT,
                                         protocol.encode_index(idx))
                    continue
                try:
                    data = f.store.read_piece(idx)
                except (OSError, protocol.ProtocolError):
                    protocol.write_frame(conn, protocol.MSG_REJECT,
                                         protocol.encode_index(idx))
                    continue
                protocol.write_frame(conn, protocol.MSG_PIECE,
                                     protocol.encode_piece_resp(idx, data))
                with f.lock:
                    f.ul += len(data)
                self._notify(f)
            elif msg_type == protocol.MSG_HELLO:
                continue  # ignore duplicate hellos
            else:
                return

    # ---- socket helpers ----

    def _connect(self, addr: str) -> socket.socket:
        host, _, port = addr.partition(":")
        sock = socket.create_connection((host, int(port)), timeout=3)
        return sock

    def _send_hello(self, sock: socket.socket, info_hash: bytes) -> None:
        sock.settimeout(5)
        protocol.write_frame(
            sock, protocol.MSG_HELLO,
            protocol.encode_hello(self._peer_id, info_hash))

    def _notify(self, f: _File) -> None:
        if self.on_progress is None:
            return
        with self._lock:
            status = self._status_locked(f)
        self.on_progress(status)

    def _finalize_if_complete(self, f: _File) -> None:
        with self._lock:
            if not f.have.complete():
                return
            try:
                f.store.finalize()
            except OSError as e:
                self._log.warning("finalize %s: %s", f.meta.name, e)
                return
        self._refresh_adverts()
        self._log.info("completed download: %s -> %s", f.meta.name, f.store.target)


def _local_addr() -> str:
    """Best-effort local IPv4 via the connect-a-UDP-socket trick."""
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        s.connect(("8.8.8.8", 80))
        return s.getsockname()[0]
    except OSError:
        return "127.0.0.1"
    finally:
        s.close()
