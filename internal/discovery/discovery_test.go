package discovery

import (
	"encoding/hex"
	"testing"
	"time"
)

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
