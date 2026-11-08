"""Wire protocol and transfer metadata, byte-compatible with the Go version.

A shared file is split into fixed-size pieces; each piece is hashed with
SHA-256 and the file is identified by the digest of the canonical metadata
(the "info hash"), exactly as in the Go implementation:

    canonical = u64(len(name)) + name + u64(length) + u64(pieceLength)
                + sha256(piece0) + sha256(piece1) + ...
    info_hash = sha256(canonical)

Frame format on TCP: ``[1-byte type][4-byte big-endian payload length][payload]``.
"""
from __future__ import annotations

import base64
import hashlib
import json
import struct
import threading
from typing import List, Optional, Tuple

# Protocol constants (must match internal/protocol/protocol.go).
VERSION = 1
DEFAULT_PIECE_SIZE = 256 * 1024
MAX_PIECE_SIZE = DEFAULT_PIECE_SIZE * 4

# Message types.
MSG_HELLO = 1
MSG_BITFIELD = 2
MSG_HAVE = 3
MSG_REQUEST = 4
MSG_PIECE = 5
MSG_GETMETA = 6
MSG_META = 7
MSG_REJECT = 8

# Frame size limits: pieces are bounded by the piece size plus slack; metadata
# frames are hex-encoded and scale with the piece count, so they get a much
# larger (but still bounded) cap.
DEFAULT_MAX_FRAME = MAX_PIECE_SIZE + 1024
META_MAX_FRAME = 64 * 1024 * 1024

_SHA256_SIZE = 32


class ProtocolError(Exception):
    """Protocol-level failure (malformed frame, hash mismatch, ...)."""


class BadFrameError(ProtocolError):
    pass


class VersionError(ProtocolError):
    pass


class PieceTooLargeError(ProtocolError):
    pass


class HashMismatchError(ProtocolError):
    pass


class Metadata:
    """Transfer metadata for one file.

    ``piece_hashes`` is a list of raw 32-byte SHA-256 digests, one per piece.
    """

    __slots__ = ("name", "length", "piece_length", "piece_hashes")

    def __init__(
        self,
        name: str,
        length: int,
        piece_length: int,
        piece_hashes: Optional[List[bytes]] = None,
    ) -> None:
        self.name = name
        self.length = length
        self.piece_length = piece_length
        self.piece_hashes: List[bytes] = piece_hashes or []

    # ---- geometry ----

    def num_pieces(self) -> int:
        if self.length <= 0:
            return 0
        return (self.length + self.piece_length - 1) // self.piece_length

    def piece_size(self, index: int) -> int:
        if index < 0 or index >= self.num_pieces():
            return 0
        rem = self.length - index * self.piece_length
        return self.piece_length if rem > self.piece_length else int(rem)

    def piece_hash(self, index: int) -> Optional[bytes]:
        if 0 <= index < len(self.piece_hashes):
            return self.piece_hashes[index]
        return None

    # ---- identity ----

    def canonical(self) -> bytes:
        parts = [
            struct.pack(">Q", len(self.name.encode("utf-8"))),
            self.name.encode("utf-8"),
            struct.pack(">Q", self.length),
            struct.pack(">Q", self.piece_length),
        ]
        parts.extend(self.piece_hashes)
        return b"".join(parts)

    def info_hash(self) -> bytes:
        return hashlib.sha256(self.canonical()).digest()

    def info_hash_hex(self) -> str:
        return self.info_hash().hex()

    # ---- verification ----

    def verify(self) -> None:
        if self.length < 0:
            raise BadFrameError("negative length")
        if self.piece_length <= 0:
            raise BadFrameError("non-positive piece length")
        if self.piece_length > MAX_PIECE_SIZE:
            raise PieceTooLargeError("piece length exceeds wire limit")
        if self.num_pieces() != len(self.piece_hashes):
            raise BadFrameError("piece hash count mismatch")
        for h in self.piece_hashes:
            if len(h) != _SHA256_SIZE:
                raise BadFrameError("bad piece hash size")

    # ---- JSON (wire form, compatible with Go encoding/json) ----

    def to_json(self) -> bytes:
        payload = {
            "name": self.name,
            "length": self.length,
            "pieceLength": self.piece_length,
            "hashes": b"".join(self.piece_hashes).hex(),
        }
        return json.dumps(payload, separators=(",", ":")).encode("utf-8")

    @classmethod
    def from_json(cls, data: bytes) -> "Metadata":
        j = json.loads(data.decode("utf-8"))
        raw = bytes.fromhex(j["hashes"])
        if len(raw) % _SHA256_SIZE != 0:
            raise BadFrameError("hash payload not a multiple of 32")
        hashes = [raw[i:i + _SHA256_SIZE] for i in range(0, len(raw), _SHA256_SIZE)]
        return cls(
            name=j["name"],
            length=int(j["length"]),
            piece_length=int(j["pieceLength"]),
            piece_hashes=hashes,
        )


# ---- framing ----


def write_frame(sock, msg_type: int, payload: bytes = b"") -> None:
    header = bytes([msg_type]) + struct.pack(">I", len(payload))
    sock.sendall(header + payload)


def read_frame(sock, max_len: int = DEFAULT_MAX_FRAME) -> Tuple[int, bytes]:
    """Read one frame. Raises BadFrameError on an oversized declared length."""
    header = _read_exactly(sock, 5)
    msg_type = header[0]
    (length,) = struct.unpack(">I", header[1:])
    if length > max_len:
        raise BadFrameError(f"frame too large: {length}")
    payload = _read_exactly(sock, length) if length else b""
    return msg_type, payload


def _read_exactly(sock, n: int) -> bytes:
    chunks = []
    remaining = n
    while remaining:
        data = sock.recv(remaining)
        if not data:
            raise BadFrameError("connection closed mid-frame")
        chunks.append(data)
        remaining -= len(data)
    return b"".join(chunks)


# ---- payload helpers ----


def encode_index(index: int) -> bytes:
    return struct.pack(">I", index)


def decode_index(payload: bytes) -> int:
    if len(payload) != 4:
        raise BadFrameError("index payload must be 4 bytes")
    (idx,) = struct.unpack(">I", payload)
    return idx


def encode_piece_resp(index: int, data: bytes) -> bytes:
    return struct.pack(">I", index) + data


def decode_piece_resp(payload: bytes) -> Tuple[int, bytes]:
    if len(payload) < 4:
        raise BadFrameError("piece payload too short")
    (idx,) = struct.unpack(">I", payload[:4])
    return idx, payload[4:]


def encode_hello(peer_id: bytes, info_hash: bytes) -> bytes:
    """Hello JSON. Go marshals []byte fields as standard base64, so we must
    too for byte-compatible interop."""
    payload = {
        "version": VERSION,
        "peerId": base64.b64encode(peer_id).decode("ascii"),
        "infoHash": base64.b64encode(info_hash).decode("ascii"),
    }
    return json.dumps(payload, separators=(",", ":")).encode("utf-8")


def decode_hello(payload: bytes) -> Tuple[int, bytes, bytes]:
    j = json.loads(payload.decode("utf-8"))
    version = int(j["version"])
    peer_id = base64.b64decode(j.get("peerId", ""))
    info_hash = base64.b64decode(j.get("infoHash", ""))
    return version, peer_id, info_hash


# ---- bitfield ----


class Bitfield:
    """Tracks which pieces are present. Thread-safe: a single node may be
    downloading pieces while serving them to other peers concurrently."""

    def __init__(self, n: int, raw: Optional[bytes] = None) -> None:
        self.n = n
        self._b = bytearray((n + 7) // 8)
        if raw:
            self._b[: len(raw)] = raw[: len(self._b)]
        self._lock = threading.Lock()

    def set(self, i: int) -> None:
        if i < 0 or i >= self.n:
            return
        with self._lock:
            self._b[i // 8] |= 1 << (i % 8)

    def has(self, i: int) -> bool:
        if i < 0 or i >= self.n:
            return False
        with self._lock:
            return bool(self._b[i // 8] & (1 << (i % 8)))

    def count(self) -> int:
        with self._lock:
            return sum(1 for i in range(self.n) if self._b[i // 8] & (1 << (i % 8)))

    def complete(self) -> bool:
        return self.count() == self.n

    def missing(self) -> List[int]:
        with self._lock:
            return [i for i in range(self.n) if not (self._b[i // 8] & (1 << (i % 8)))]
