package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
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

func TestReadFrameRejectsOversized(t *testing.T) {
	// A length field just over the piece-frame cap must be rejected without
	// allocating the claimed payload (a cheap remote DoS would be to claim a
	// near-4 GiB frame). Write a header claiming DefaultMaxFrame+1 bytes with
	// only a tiny amount of body after it; ReadFrame must fail on the length,
	// not block reading gigabytes.
	var buf bytes.Buffer
	header := make([]byte, 5)
	header[0] = byte(MsgPiece)
	binary.BigEndian.PutUint32(header[1:], DefaultMaxFrame+1)
	buf.Write(header)
	buf.Write([]byte("tiny"))
	if _, _, err := ReadFrame(&buf); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("expected ErrBadFrame for oversized frame, got %v", err)
	}
}

func TestReadFrameWithMaxAllowsLargeMeta(t *testing.T) {
	// Regression: metadata for a large file is hex-encoded and can be far
	// bigger than the piece-frame cap, so it must be readable with the larger
	// metadata limit. Build a payload larger than DefaultMaxFrame but well
	// within MetaMaxFrame and confirm it round-trips only under the big limit.
	big := make([]byte, DefaultMaxFrame+1)
	for i := range big {
		big[i] = byte(i)
	}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, MsgMeta, big); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadFrame(bytes.NewReader(buf.Bytes())); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("default ReadFrame should reject a meta-sized frame, got %v", err)
	}
	var buf2 bytes.Buffer
	if err := WriteFrame(&buf2, MsgMeta, big); err != nil {
		t.Fatal(err)
	}
	gotType, got, err := ReadFrameWithMax(&buf2, MetaMaxFrame)
	if err != nil {
		t.Fatalf("ReadFrameWithMax(MetaMaxFrame) failed: %v", err)
	}
	if gotType != MsgMeta || !bytes.Equal(got, big) {
		t.Fatal("large metadata frame did not round trip")
	}
}

func TestMaliciousPieceRespBounds(t *testing.T) {
	// A peer can put any index in a piece response, including one out of range.
	// DecodePieceResp only parses; safety comes from store.WritePiece rejecting
	// an index whose expected piece size is 0. Here we assert the parse does not
	// panic and surfaces the index so the caller can reject it.
	if _, _, err := DecodePieceResp([]byte{0, 0}); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("short resp should be ErrBadFrame, got %v", err)
	}
	// A huge index with no data: parses fine (caller must bounds-check via the
	// metadata before trusting it).
	enc := EncodePieceResp(1<<30, nil)
	idx, data, err := DecodePieceResp(enc)
	if err != nil || idx != 1<<30 || len(data) != 0 {
		t.Fatalf("huge-index resp parse wrong: idx=%d len=%d err=%v", idx, len(data), err)
	}
}

func TestVerifyRejectsOversizedPieceLength(t *testing.T) {
	// A metadata whose piece length exceeds MaxPieceSize would produce piece
	// frames larger than DefaultMaxFrame, which peers cannot receive. Verify must
	// reject it so BuildMetadata fails at share time and GetMeta rejects a peer
	// advertising such metadata.
	m := &Metadata{Name: "big", Length: 10, PieceLength: MaxPieceSize + 1}
	if err := m.Verify(); !errors.Is(err, ErrPieceTooLarge) {
		t.Fatalf("Verify with oversized piece = %v, want ErrPieceTooLarge", err)
	}
	// Exactly at the limit is allowed.
	m.PieceLength = MaxPieceSize
	// Give it the right number of hashes so the size check is the only failure.
	n := m.NumPieces()
	for len(m.PieceHashes) < n {
		h := sha256.Sum256(nil)
		m.PieceHashes = append(m.PieceHashes, h[:])
	}
	if err := m.Verify(); err != nil {
		t.Fatalf("Verify at exactly MaxPieceSize should pass, got %v", err)
	}
}

func TestBitfieldSetOutOfRangeGuard(t *testing.T) {
	// Has/Missing/Count must be safe for out-of-range queries (a remote peer
	// could advertise arbitrary indices). Has already bounds-checks; assert it
	// never panics and reports false for out-of-range.
	bf := NewBitfield(5)
	for _, i := range []int{-1, 5, 100, 1 << 20} {
		if bf.Has(i) {
			t.Fatalf("Has(%d) should be false on a 5-piece bitfield", i)
		}
	}
}
