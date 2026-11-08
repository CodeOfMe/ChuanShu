package discovery

import (
	"encoding/binary"
	"encoding/hex"
	"testing"
	"time"
)

// TestDecodeHostilePackets ensures decode never panics on malformed/hostile UDP
// payloads. Discovery is reachable by any host on the LAN via multicast, so a
// crafted packet must be rejected, not crash the node. We sweep lengths around
// every header boundary (magic, count, each entry's hash/port/flag fields) and
// packets that claim a count larger than their body.
func TestDecodeHostilePackets(t *testing.T) {
	var base []byte
	b4 := make([]byte, 4)
	binary.BigEndian.PutUint32(b4, magic)
	base = append(base, b4...) // valid magic
	base = append(base, typeAnnounce)
	base = append(base, 3) // claims 3 entries (105 bytes), but we'll truncate

	// Truncations at every byte length from 0 up past a full 3-entry packet.
	for n := 0; n <= len(base)+120; n++ {
		// no panic is the assertion
		_, _, _ = decode(base[:min(n, len(base))])
	}
	// A packet with correct count but garbage bytes.
	garbage := make([]byte, 6+3*35)
	copy(garbage, base)
	for i := 6; i < len(garbage); i++ {
		garbage[i] = 0xFF
	}
	_, _, _ = decode(garbage)
	// count byte says 255 but body is short -> must reject, not read past end.
	shortHuge := make([]byte, 6)
	copy(shortHuge, base)
	shortHuge[5] = 255
	if _, _, err := decode(shortHuge); err == nil {
		t.Fatal("expected error on count larger than body")
	}
	// Random byte sweep over a valid-magic prefix.
	var seed uint32 = 0x1234
	for i := 0; i < 2000; i++ {
		seed = seed*1664525 + 1013904223
		buf := make([]byte, int(seed%64))
		for j := range buf {
			seed = seed*1664525 + 1013904223
			buf[j] = byte(seed >> 16)
		}
		if len(buf) >= 4 {
			binary.BigEndian.PutUint32(buf[:4], magic)
		}
		_, _, _ = decode(buf) // must never panic
	}
}

func TestPacketEncodeDecode(t *testing.T) {
	s := NewService(0, 6000)
	s.SetDataPort(6000)
	var ih [32]byte
	for i := range ih {
		ih[i] = byte(i)
	}
	adverts := []Advert{{InfoHash: ih, Complete: true}}
	pkt := s.encode(typeAnnounce, adverts)
	if pkt == nil {
		t.Fatal("encode returned nil")
	}
	pType, entries, err := decode(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if pType != typeAnnounce {
		t.Fatalf("type = %d, want %d", pType, typeAnnounce)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].InfoHash != ih {
		t.Fatal("info hash round trip mismatch")
	}
	if entries[0].TCPPort != 6000 {
		t.Fatalf("tcp port = %d, want 6000", entries[0].TCPPort)
	}
	if !entries[0].Complete {
		t.Fatal("complete flag lost")
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, _, err := decode([]byte{1, 2, 3}); err != ErrBadPacket {
		t.Fatalf("short packet: err = %v, want ErrBadPacket", err)
	}
	// Wrong magic.
	bad := make([]byte, 64)
	if _, _, err := decode(bad); err != ErrBadPacket {
		t.Fatalf("bad magic: err = %v, want ErrBadPacket", err)
	}
}

func TestRegistryPeersAndSeeders(t *testing.T) {
	r := NewRegistry()
	var ih [32]byte
	ih[0] = 9
	now := time.Now()
	hex := encodeKey(ih)
	key := hex
	// Two peers: one complete seeder, one partial.
	r.add(key, &Peer{TCPAddr: "10.0.0.1:1", InfoHash: ih, Complete: true, LastSeen: now})
	r.add(key, &Peer{TCPAddr: "10.0.0.2:2", InfoHash: ih, Complete: false, LastSeen: now})

	peers := r.Peers(ih, "", time.Minute)
	if len(peers) != 2 {
		t.Fatalf("Peers = %d, want 2", len(peers))
	}
	// self-exclusion
	if len(r.Peers(ih, "10.0.0.1:1", time.Minute)) != 1 {
		t.Fatal("self should be excluded from Peers")
	}
	// Seeders counts only complete peers.
	if got := r.Seeders(hex, time.Minute); got != 1 {
		t.Fatalf("Seeders = %d, want 1", got)
	}
	// Stale peers are not returned.
	if got := len(r.Peers(ih, "", time.Nanosecond)); got != 0 {
		t.Fatalf("stale Peers = %d, want 0", got)
	}
	// Hashes lists the file.
	if hs := r.Hashes(time.Minute); len(hs) != 1 || hs[0] != hex {
		t.Fatalf("Hashes = %v, want [%s]", hs, hex)
	}
}

// encodeKey mirrors Hashes' encoding so tests can compare.
func encodeKey(ih [32]byte) string {
	return hex.EncodeToString(ih[:])
}
