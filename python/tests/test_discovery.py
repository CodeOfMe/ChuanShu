"""Tests for discovery: packet codec and live registry wiring.

These use the real UDP multicast socket on an ephemeral discovery port (passed
explicitly) so running the suite does not interfere with anything else on the
default port.
"""
import time

import pytest

from chuanshu import discovery


def test_encode_decode_roundtrip():
    ih = bytes(range(32))
    adverts = [discovery.Advert(ih, True)]
    pkt = discovery.encode_packet(discovery.TYPE_ANNOUNCE, 40000, adverts)
    # magic(4) + type(1) + count(1) + 35 bytes per advert
    assert len(pkt) == 6 + 35
    ptype, entries = discovery.decode_packet(pkt)
    assert ptype == discovery.TYPE_ANNOUNCE
    assert entries == [(ih, 40000, True)]


def test_encode_multiple_and_flags():
    a = bytes([1] * 32)
    b = bytes([2] * 32)
    pkt = discovery.encode_packet(discovery.TYPE_REPLY, 41000,
                                  [discovery.Advert(a, False),
                                   discovery.Advert(b, True)])
    ptype, entries = discovery.decode_packet(pkt)
    assert ptype == discovery.TYPE_REPLY
    assert (a, 41000, False) in entries
    assert (b, 41000, True) in entries


def test_encode_empty_returns_none():
    assert discovery.encode_packet(discovery.TYPE_ANNOUNCE, 40000, []) is None


def test_decode_rejects_bad():
    with pytest.raises(ValueError):
        discovery.decode_packet(b"short")
    with pytest.raises(ValueError):
        discovery.decode_packet(b"\x00" * 40)  # wrong magic
    # valid header claiming 2 entries but with a truncated body
    header = discovery.encode_packet(discovery.TYPE_ANNOUNCE, 40000,
                                     [discovery.Advert(bytes(32), True)])
    with pytest.raises(ValueError):
        discovery.decode_packet(header[:20])  # shorter than one full entry


def test_registry_sticky_complete():
    reg = discovery.Registry()
    now = time.time()
    reg.add("aa", discovery.Peer("1.2.3.4:1", bytes(32), False, now))
    reg.add("aa", discovery.Peer("1.2.3.4:1", bytes(32), True, now))
    reg.add("aa", discovery.Peer("1.2.3.4:1", bytes(32), False, now))
    peers = reg.peers("aa", self_addr="", max_age=60)
    assert len(peers) == 1 and peers[0].complete


def test_live_discovery_between_services():
    """Two Services on a shared ephemeral port discover each other's adverts."""
    ih = bytes(range(32))
    port = 51500
    svc_a = discovery.Service(udp_port=port)
    svc_b = discovery.Service(udp_port=port)
    try:
        svc_a.set_data_port(40001)
        svc_b.set_data_port(40002)
        svc_a.set_advertised([discovery.Advert(ih, True)])
        svc_b.set_advertised([discovery.Advert(ih, True)])
        svc_a.start()
        svc_b.start()
        deadline = time.time() + 5
        found = False
        while time.time() < deadline:
            svc_a._broadcast_all()
            svc_b._broadcast_all()
            peers_a = svc_a.registry.peers(ih.hex(), self_addr="", max_age=60)
            if any(p.tcp_addr.endswith(":40002") for p in peers_a):
                found = True
                break
            time.sleep(0.2)
        assert found, "discovery did not see the peer's advertised TCP port"
    finally:
        svc_a.stop()
        svc_b.stop()
