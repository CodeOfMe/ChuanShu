package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/CodeOfMe/ChuanShu/internal/protocol"
)

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildMetadataAndReadPieces(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	// 1000 bytes with piece length 300 => 4 pieces (300,300,300,100).
	data := make([]byte, 1000)
	for i := range data {
		data[i] = byte(i % 251)
	}
	writeFile(t, src, data)

	meta, _, err := BuildMetadata(src, 300)
	if err != nil {
		t.Fatal(err)
	}
	if err := meta.Verify(); err != nil {
		t.Fatal(err)
	}
	if meta.NumPieces() != 4 {
		t.Fatalf("NumPieces = %d, want 4", meta.NumPieces())
	}

	st, err := OpenExisting(src, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Each piece must read back with its expected hash and HasPiece true.
	for i := 0; i < meta.NumPieces(); i++ {
		got, err := st.ReadPiece(i)
		if err != nil {
			t.Fatalf("ReadPiece(%d): %v", i, err)
		}
		want := data[int64(i)*int64(meta.PieceLength) : int64(i)*int64(meta.PieceLength)+int64(meta.PieceSize(i))]
		if !bytes.Equal(got, want) {
			t.Fatalf("piece %d mismatch", i)
		}
		if !st.HasPiece(i) {
			t.Fatalf("HasPiece(%d) should be true", i)
		}
	}
}

func TestWritePieceRejectsBadHash(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	data := []byte("0123456789")
	writeFile(t, src, data)
	meta, _, err := BuildMetadata(src, 4)
	if err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	dst, err := CreateDownload(outDir, meta)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	// A correct piece should be accepted.
	if err := dst.WritePiece(0, data[0:4]); err != nil {
		t.Fatalf("WritePiece(0) valid: %v", err)
	}
	// A wrong piece should be rejected with ErrHashMismatch.
	if err := dst.WritePiece(1, []byte("XXXX")); err != protocol.ErrHashMismatch {
		t.Fatalf("WritePiece(1) bad = %v, want ErrHashMismatch", err)
	}
}

// TestWritePieceRejectsOutOfRange guards against a malicious peer sending a
// piece response that names an index outside the file (pieceSize==0). It must be
// rejected as a hash mismatch and must never WriteAt past the truncated length.
func TestWritePieceRejectsOutOfRange(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	data := []byte("0123456789")
	writeFile(t, src, data)
	meta, _, err := BuildMetadata(src, 4) // 3 pieces: 4,4,2
	if err != nil {
		t.Fatal(err)
	}
	dst, err := CreateDownload(t.TempDir(), meta)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	// Nonexistent index with plausible data (size 4) and empty payload.
	if err := dst.WritePiece(99, []byte("XXXX")); err != protocol.ErrHashMismatch {
		t.Fatalf("WritePiece(99) = %v, want ErrHashMismatch", err)
	}
	if err := dst.WritePiece(99, nil); err != protocol.ErrHashMismatch {
		t.Fatalf("WritePiece(99,nil) = %v, want ErrHashMismatch", err)
	}
	// Negative index too.
	if err := dst.WritePiece(-1, data[0:4]); err != protocol.ErrHashMismatch {
		t.Fatalf("WritePiece(-1) = %v, want ErrHashMismatch", err)
	}
}

func TestCreateDownloadFinalizeAndResume(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	data := make([]byte, 900)
	for i := range data {
		data[i] = byte(i)
	}
	writeFile(t, src, data)
	meta, _, err := BuildMetadata(src, 500)
	if err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	st, err := CreateDownload(outDir, meta)
	if err != nil {
		t.Fatal(err)
	}
	// Piece 1 is written, piece 0 remains a gap.
	if err := st.WritePiece(1, data[500:900]); err != nil {
		t.Fatal(err)
	}
	if st.HasPiece(0) {
		t.Fatal("piece 0 should be missing (zero-filled)")
	}
	if !st.HasPiece(1) {
		t.Fatal("piece 1 should be present")
	}
	// Simulate resume: reopen the partial and detect piece 1 via HasPiece.
	st.Close()
	reopen, err := os.OpenFile(st.partial, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	rst := &Store{Meta: meta, file: reopen, partial: st.partial, Target: st.Target}
	if !rst.HasPiece(1) {
		t.Fatal("resume did not detect already-written piece 1")
	}
	rst.Close()
}
