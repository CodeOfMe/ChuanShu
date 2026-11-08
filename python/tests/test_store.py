"""Tests for piece IO: build_metadata, sanitize, Store."""
import hashlib
import os

import pytest

from chuanshu import protocol, store


def test_build_metadata_pieces_and_hash(tmp_path):
    data = os.urandom(5000)
    p = tmp_path / "file.bin"
    p.write_bytes(data)
    meta = store.build_metadata(str(p), 1024)
    assert meta.name == "file.bin"
    assert meta.length == 5000
    assert meta.piece_length == 1024
    assert meta.num_pieces() == 5
    # Each piece's hash is SHA-256 of its slice.
    for i in range(5):
        chunk = data[i * 1024:(i + 1) * 1024]
        assert meta.piece_hash(i) == hashlib.sha256(chunk).digest()


def test_build_metadata_default_piece_size(tmp_path):
    p = tmp_path / "small.bin"
    p.write_bytes(b"x" * 10)
    meta = store.build_metadata(str(p))
    assert meta.piece_length == protocol.DEFAULT_PIECE_SIZE
    assert meta.num_pieces() == 1


def test_sanitize_rejects_traversal():
    assert store.sanitize_name("ok.bin") == "ok.bin"
    for bad in ["", ".", "..", "a/b", "a\\b", "/abs", "c:\\win"]:
        with pytest.raises(protocol.BadFrameError):
            store.sanitize_name(bad)


def test_store_verified_write_and_finalize(tmp_path):
    data = os.urandom(4096)
    src = tmp_path / "src.bin"
    src.write_bytes(data)
    meta = store.build_metadata(str(src), 1024)

    # Simulate a download: create a partial file and stream pieces in reverse.
    dl = tmp_path / "dl"
    st = store.Store.create_download(str(dl), meta)
    for i in reversed(range(meta.num_pieces())):
        st.write_piece(i, data[i * 1024:(i + 1) * 1024])
    st.finalize()
    assert (dl / "src.bin").read_bytes() == data
    assert not os.path.exists(str(dl / "src.bin") + ".part")


def test_store_rejects_bad_piece(tmp_path):
    data = os.urandom(2048)
    src = tmp_path / "src.bin"
    src.write_bytes(data)
    meta = store.build_metadata(str(src), 1024)
    st = store.Store.create_download(str(tmp_path / "dl"), meta)
    with pytest.raises(protocol.HashMismatchError):
        st.write_piece(0, b"\x00" * 1024)
    st.delete_partial()
