"""Piece IO: hashing source files, verified reads/writes, resume support.

The download target is created at full length up front (sparse on most
filesystems) so pieces can be written at their correct offsets regardless of
arrival order. Each piece is verified against its expected hash before it is
written, so a corrupt or hostile peer can never corrupt the output.
"""
from __future__ import annotations

import hashlib
import os
import threading
from pathlib import Path

from . import protocol

_HAS_PREAD = hasattr(os, "pread") and hasattr(os, "pwrite")


def _pread(fd: int, size: int, offset: int, lock=None) -> bytes:
    """Positional read: pread where available (POSIX), lseek+read on Windows."""
    if _HAS_PREAD:
        data = os.pread(fd, size, offset)
        while len(data) < size:
            more = os.pread(fd, size - len(data), offset + len(data))
            if not more:
                break
            data += more
        return data
    with lock:
        os.lseek(fd, offset, os.SEEK_SET)
        chunks = []
        remaining = size
        while remaining:
            chunk = os.read(fd, remaining)
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        return b"".join(chunks)


def _pwrite(fd: int, data: bytes, offset: int, lock=None) -> None:
    """Positional write: pwrite where available (POSIX), lseek+write on Windows."""
    if _HAS_PREAD:
        written = 0
        while written < len(data):
            written += os.pwrite(fd, data[written:], offset + written)
        return
    with lock:
        os.lseek(fd, offset, os.SEEK_SET)
        written = 0
        while written < len(data):
            written += os.write(fd, data[written:])


def build_metadata(path: str, piece_len: int = 0) -> protocol.Metadata:
    """Hash a source file into transfer metadata, streaming so memory stays
    flat even for very large files."""
    if piece_len <= 0:
        piece_len = protocol.DEFAULT_PIECE_SIZE
    p = Path(path)
    st = p.stat()
    if not p.is_file():
        raise IsADirectoryError(f"not a regular file: {path}")

    hashes = []
    with open(path, "rb") as f:
        while True:
            buf = f.read(piece_len)
            if not buf:
                break
            hashes.append(hashlib.sha256(buf).digest())

    meta = protocol.Metadata(
        name=p.name,  # mirrors Go filepath.Base
        length=st.st_size,
        piece_length=piece_len,
        piece_hashes=hashes,
    )
    meta.verify()
    return meta


def sanitize_name(name: str) -> str:
    """Guard the download path against traversal via a peer-supplied Name.

    Legitimate names are ``basename`` output and contain no separators. This
    is defense in depth: the info hash already binds the name, forging one
    would require a SHA-256 collision.
    """
    if not name or name in (".", ".."):
        raise protocol.BadFrameError("invalid file name")
    if os.path.basename(name) != name:
        raise protocol.BadFrameError("name is not a single path element")
    if "/" in name or "\\" in name:
        raise protocol.BadFrameError("name contains a separator")
    return name


class Store:
    """Local copy of one file, either an existing source (seeding) or a
    download in progress living at ``target.part`` until completion.

    Piece IO uses ``os.pread``/``os.pwrite`` (positional, atomic) so multiple
    threads can read and write different pieces concurrently, mirroring the
    Go implementation's ReadAt/WriteAt.
    """

    def __init__(self, meta: protocol.Metadata, target: str,
                 fd=None, partial: str = "") -> None:
        self.meta = meta
        self.target = target
        self._fd = fd
        self._partial = partial
        # Only used on platforms without pread/pwrite (Windows).
        self._io_lock = threading.Lock()

    # ---- constructors ----

    @classmethod
    def open_existing(cls, path: str, meta: protocol.Metadata) -> "Store":
        fd = os.open(path, os.O_RDONLY)
        return cls(meta, path, fd=fd)

    @classmethod
    def create_download(cls, dir_path: str, meta: protocol.Metadata) -> "Store":
        os.makedirs(dir_path, exist_ok=True)
        name = sanitize_name(meta.name)
        target = os.path.join(dir_path, name)
        partial = target + ".part"
        # Create without truncating so an interrupted download can resume.
        fd = os.open(partial, os.O_RDWR | os.O_CREAT, 0o644)
        if os.fstat(fd).st_size < meta.length:
            os.truncate(fd, meta.length)
        return cls(meta, target, fd=fd, partial=partial)

    # ---- piece IO ----

    def read_piece(self, index: int) -> bytes:
        size = self.meta.piece_size(index)
        if size == 0:
            raise protocol.BadFrameError(f"no such piece: {index}")
        data = _pread(self._fd, size, index * self.meta.piece_length, self._io_lock)
        if len(data) != size:
            raise IOError("short read")
        return data

    def has_piece(self, index: int) -> bool:
        """True when the piece on disk matches its expected hash (resume)."""
        try:
            data = self.read_piece(index)
        except (OSError, ValueError):
            return False
        want = self.meta.piece_hash(index)
        if not want or len(want) != 32:
            return False
        return hashlib.sha256(data).digest() == want

    def write_piece(self, index: int, data: bytes) -> None:
        size = self.meta.piece_size(index)
        if len(data) != size:
            raise protocol.HashMismatchError("piece size mismatch")
        want = self.meta.piece_hash(index)
        if hashlib.sha256(data).digest() != want:
            raise protocol.HashMismatchError("piece hash mismatch")
        _pwrite(self._fd, data, index * self.meta.piece_length, self._io_lock)

    # ---- lifecycle ----

    def close(self) -> None:
        if self._fd is not None:
            try:
                os.close(self._fd)
            except OSError:
                pass
            self._fd = None

    def delete_partial(self) -> None:
        self.close()
        if self._partial:
            try:
                os.remove(self._partial)
            except OSError:
                pass
            self._partial = ""

    def finalize(self) -> None:
        if not self._partial:
            return
        os.fsync(self._fd)
        os.close(self._fd)
        os.rename(self._partial, self.target)
        self._fd = None
        self._partial = ""
