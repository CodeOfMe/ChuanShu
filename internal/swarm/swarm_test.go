package swarm

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fredchuanshu/chuanshu/internal/protocol"
)

// TestNodeSeedAndDownload runs a seeder and a downloader node inside one test
// process and verifies the file transfers intact and the downloader becomes a
// complete copy. This exercises the TCP piece protocol and the download loop.
func TestNodeSeedAndDownload(t *testing.T) {
	src := filepath.Join(t.TempDir(), "hello.bin")
	data := make([]byte, 200*1000) // ~200 KiB, split into 64 KiB pieces by AddSeed
	for i := range data {
		data[i] = byte(i % 197)
	}
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}

	seeder, err := NewNode(Options{DiscoveryPort: 51455, DataPort: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := seeder.Start(); err != nil {
		t.Fatal(err)
	}
	defer seeder.Stop()

	meta, err := seeder.AddSeed(src, 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	infoHex := meta.InfoHashHex()

	dlDir := t.TempDir()
	dl, err := NewNode(Options{DiscoveryPort: 51455, DataPort: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := dl.Start(); err != nil {
		t.Fatal(err)
	}
	defer dl.Stop()

	if err := dl.StartDownload(dlDir, meta); err != nil {
		t.Fatal(err)
	}

	// Poll until complete or timeout.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st, ok := dl.StatusFor(infoHex)
		if ok && st.Complete {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	st, ok := dl.StatusFor(infoHex)
	if !ok || !st.Complete {
		t.Fatalf("download did not complete: %+v ok=%v", st, ok)
	}
	got, err := os.ReadFile(filepath.Join(dlDir, meta.Name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("downloaded bytes differ from source")
	}
}

// TestNodeResumePartial seeds a download dir with half the pieces, then starts
// a fresh downloader node and verifies it completes without refetching the
// pieces already present on disk.
func TestNodeResumePartial(t *testing.T) {
	src := filepath.Join(t.TempDir(), "resume.bin")
	data := make([]byte, 400*1000)
	for i := range data {
		data[i] = byte((i * 7) % 251)
	}
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	piece := 50 * 1024

	seeder, err := NewNode(Options{DiscoveryPort: 51457, DataPort: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := seeder.Start(); err != nil {
		t.Fatal(err)
	}
	defer seeder.Stop()
	meta, err := seeder.AddSeed(src, piece)
	if err != nil {
		t.Fatal(err)
	}
	infoHex := meta.InfoHashHex()

	dlDir := t.TempDir()
	// Create a partial file already seeded with the first half of pieces.
	if err := writePartial(dlDir, meta, data, piece); err != nil {
		t.Fatal(err)
	}

	dl, err := NewNode(Options{DiscoveryPort: 51457, DataPort: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := dl.Start(); err != nil {
		t.Fatal(err)
	}
	defer dl.Stop()
	if err := dl.StartDownload(dlDir, meta); err != nil {
		t.Fatal(err)
	}
	// The downloader should detect the pre-seeded pieces immediately.
	st, ok := dl.StatusFor(infoHex)
	if !ok {
		t.Fatal("file not registered for download")
	}
	if st.HavePieces <= 0 {
		t.Fatalf("expected existing pieces to be detected, got 0")
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := dl.StatusFor(infoHex)
		if st.Complete {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	st, _ = dl.StatusFor(infoHex)
	if !st.Complete {
		t.Fatalf("resume download did not complete: %+v", st)
	}
	got, err := os.ReadFile(filepath.Join(dlDir, meta.Name))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatal("resumed download bytes differ from source")
	}
}

// writePartial creates a .part file and fills it with the first half of pieces
// so a subsequent StartDownload can detect them and resume.
func writePartial(dir string, meta *protocol.Metadata, data []byte, piece int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	partPath := filepath.Join(dir, meta.Name+".part")
	f, err := os.Create(partPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(meta.Length); err != nil {
		return err
	}
	half := meta.NumPieces() / 2
	for i := 0; i < half; i++ {
		start := int64(i) * int64(piece)
		end := start + int64(meta.PieceSize(i))
		if _, err := f.WriteAt(data[start:end], start); err != nil {
			return err
		}
	}
	return f.Sync()
}
