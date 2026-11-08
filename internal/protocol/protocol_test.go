package protocol

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

func TestPieceSizing(t *testing.T) {
	m := &Metadata{Length: 1000, PieceLength: 256}
	if got := m.NumPieces(); got != 4 {
		t.Fatalf("NumPieces = %d, want 4", got)
	}
	if got := m.PieceSize(0); got != 256 {
		t.Fatalf("PieceSize(0) = %d, want 256", got)
	}
	if got := m.PieceSize(3); got != 232 {
		t.Fatalf("PieceSize(3) = %d, want 232 (remainder)", got)
	}
	if got := m.PieceSize(4); got != 0 {
		t.Fatalf("PieceSize(4) = %d, want 0 (out of range)", got)
	}
}

func TestInfoHashStableAndDistinct(t *testing.T) {
	hashA := make([]byte, sha256.Size)
	hashB := make([]byte, sha256.Size)
	m1 := &Metadata{Name: "a", Length: 10, PieceLength: 5, PieceHashes: [][]byte{hashA}}
	m2 := &Metadata{Name: "a", Length: 10, PieceLength: 5, PieceHashes: [][]byte{hashA}}
	m3 := &Metadata{Name: "b", Length: 10, PieceLength: 5, PieceHashes: [][]byte{hashB}}

	if m1.InfoHashHex() != m2.InfoHashHex() {
		t.Fatal("identical metadata produced different info hashes")
	}
	if m1.InfoHashHex() == m3.InfoHashHex() {
		t.Fatal("different metadata produced identical info hashes")
	}
}

func TestMetadataJSONRoundTrip(t *testing.T) {
	orig := &Metadata{
		Name:        "hello.bin",
		Length:      1000,
		PieceLength: 512,
	}
	for i := 0; i < orig.NumPieces(); i++ {
		h := sha256.Sum256([]byte{byte(i)})
		d := make([]byte, sha256.Size)
		copy(d, h[:])
		orig.PieceHashes = append(orig.PieceHashes, d)
	}
	b, err := orig.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var got Metadata
	if err := got.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	if got.InfoHashHex() != orig.InfoHashHex() {
		t.Fatalf("info hash changed after round trip: %s vs %s", got.InfoHashHex(), orig.InfoHashHex())
	}
	if err := got.Verify(); err != nil {
		t.Fatalf("Verify after round trip: %v", err)
	}
}

func TestBitfield(t *testing.T) {
	bf := NewBitfield(10)
	if !bf.Has(0) && bf.Count() != 0 {
		t.Fatal("new bitfield should be empty")
	}
	bf.Set(3)
	bf.Set(9)
	if !bf.Has(3) || !bf.Has(9) || bf.Has(4) {
		t.Fatal("Has() incorrect after Set")
	}
	if bf.Count() != 2 {
		t.Fatalf("Count = %d, want 2", bf.Count())
	}
	if bf.Complete() {
		t.Fatal("should not be complete")
	}
	missing := bf.Missing()
	if len(missing) != 8 {
		t.Fatalf("Missing len = %d, want 8", len(missing))
	}
	// Rebuild from bytes and confirm preservation.
	rt := BitfieldFromBytes(bf.Bytes(), 10)
	if rt.Count() != 2 || !rt.Has(3) || !rt.Has(9) {
		t.Fatal("round trip via Bytes/BitfieldFromBytes lost bits")
	}
}

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("piece-data-123")
	if err := WriteFrame(&buf, MsgPiece, payload); err != nil {
		t.Fatal(err)
	}
	gotType, gotPayload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if gotType != MsgPiece {
		t.Fatalf("type = %d, want %d", gotType, MsgPiece)
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Fatalf("payload = %q, want %q", gotPayload, payload)
	}
}

func TestPieceRespCodec(t *testing.T) {
	data := []byte("hello world piece")
	enc := EncodePieceResp(42, data)
	idx, dec, err := DecodePieceResp(enc)
	if err != nil {
		t.Fatal(err)
	}
	if idx != 42 || !bytes.Equal(dec, data) {
		t.Fatalf("round trip mismatch: idx=%d data=%q", idx, dec)
	}
}
