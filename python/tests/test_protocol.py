"""Tests for the wire protocol: identity, framing, hello, bitfield."""
import json
import struct

import pytest

from chuanshu import protocol


def _meta(name="f.bin", length=0, piece_length=1024, hashes=None):
    return protocol.Metadata(name, length, piece_length, hashes or [])


def test_canonical_layout_matches_go():
    # canonical = u64(len(name)) + name + u64(length) + u64(pieceLength) + hashes
    h = b"\x01" * 32
    m = _meta(name="abc", length=5, piece_length=2, hashes=[h])
    expect = struct.pack(">Q", 3) + b"abc" + struct.pack(">Q", 5) \
        + struct.pack(">Q", 2) + h
    assert m.canonical() == expect


def test_info_hash_stable_and_hex():
    m = _meta(name="x", length=10, piece_length=4,
              hashes=[bytes([i]) * 32 for i in range(3)])
    digest = m.info_hash()
    assert len(digest) == 32
    assert m.info_hash_hex() == digest.hex()
    # identical inputs give identical hash
    m2 = _meta(name="x", length=10, piece_length=4,
               hashes=[bytes([i]) * 32 for i in range(3)])
    assert m2.info_hash() == digest


def test_num_pieces_and_piece_size():
    m = _meta(length=10, piece_length=4)
    assert m.num_pieces() == 3
    assert [m.piece_size(i) for i in range(3)] == [4, 4, 2]
    assert m.piece_size(-1) == 0
    assert m.piece_size(3) == 0
    assert _meta(length=0).num_pieces() == 0


def test_verify_rejects_bad():
    with pytest.raises(protocol.BadFrameError):
        _meta(length=-1).verify()
    with pytest.raises(protocol.BadFrameError):
        _meta(piece_length=0).verify()
    with pytest.raises(protocol.PieceTooLargeError):
        _meta(piece_length=protocol.MAX_PIECE_SIZE + 1).verify()
    with pytest.raises(protocol.BadFrameError):
        _meta(length=10, piece_length=4, hashes=[b"short"]).verify()


def test_metadata_json_roundtrip():
    hashes = [bytes([i]) * 32 for i in range(3)]
    m = protocol.Metadata("文件.bin", 10, 4, hashes)
    payload = m.to_json()
    assert json.loads(payload)["name"] == "文件.bin"
    m2 = protocol.Metadata.from_json(payload)
    assert m2.info_hash() == m.info_hash()
    assert m2.name == "文件.bin"


def test_hello_uses_base64_for_bytes():
    peer_id = bytes(range(8))
    ih = bytes(range(32))
    payload = protocol.encode_hello(peer_id, ih)
    j = json.loads(payload)
    import base64
    assert j["peerId"] == base64.b64encode(peer_id).decode()
    assert j["infoHash"] == base64.b64encode(ih).decode()
    assert j["version"] == protocol.VERSION
    ver, pid, got_ih = protocol.decode_hello(payload)
    assert (ver, pid, got_ih) == (protocol.VERSION, peer_id, ih)


def test_bitfield():
    bf = protocol.Bitfield(10)
    assert bf.count() == 0 and not bf.complete()
    bf.set(0)
    bf.set(7)
    bf.set(9)
    assert bf.has(0) and bf.has(7) and bf.has(9)
    assert bf.count() == 3
    assert bf.missing() == [1, 2, 3, 4, 5, 6, 8]
    bf.set(100)  # out of range ignored
    assert bf.count() == 3
    for i in range(10):
        bf.set(i)
    assert bf.complete()


def test_bitfield_from_bytes():
    bf = protocol.Bitfield(10, raw=b"\x03")  # bits 0,1
    assert bf.has(0) and bf.has(1) and not bf.has(2)
