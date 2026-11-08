package swarm

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CodeOfMe/ChuanShu/internal/protocol"
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

func TestNodeEmptyFile(t *testing.T) {
	// Regression: an empty file (0 pieces) was reported as Done but the .part
	// file was never renamed because the only finalization hook is triggered by
	// the arrival of a piece, and an empty file never receives one. StartDownload
	// must finalize immediately when the download starts already-complete.
	src := filepath.Join(t.TempDir(), "empty.bin")
	if err := os.WriteFile(src, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	seeder, err := NewNode(Options{DiscoveryPort: 51459, DataPort: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := seeder.Start(); err != nil {
		t.Fatal(err)
	}
	defer seeder.Stop()
	meta, err := seeder.AddSeed(src, 1024)
	if err != nil {
		t.Fatal(err)
	}
	dlDir := t.TempDir()
	dl, err := NewNode(Options{DiscoveryPort: 51459, DataPort: 0})
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
	st, ok := dl.StatusFor(meta.InfoHashHex())
	if !ok || !st.Complete {
		t.Fatalf("expected complete immediately: %+v ok=%v", st, ok)
	}
	got, err := os.ReadFile(filepath.Join(dlDir, meta.Name))
	if err != nil {
		t.Fatalf("final file missing after StartDownload: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0-byte file, got %d bytes", len(got))
	}
}

// TestGetMetaLargeMetadata exercises the metadata-retrieval path with metadata
// whose wire frame is larger than the default piece-frame cap. Before the fix,
// GetMeta read the metadata frame with ReadFrame's ~1 MiB limit, so any file
// with more than ~16k pieces (about 4 GiB at the default piece size) could never
// be downloaded: its metadata frame was rejected as a malformed frame. Here a
// tiny file split into 1-byte pieces produces a huge metadata frame cheaply, so
// we can assert GetMeta now succeeds over a real TCP connection.
func TestGetMetaLargeMetadata(t *testing.T) {
	const pieces = 17000 // 17000 * 64 hex chars ~= 1.06 MB > DefaultMaxFrame
	src := filepath.Join(t.TempDir(), "manypieces.bin")
	data := make([]byte, pieces)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}

	seeder, err := NewNode(Options{DiscoveryPort: 51461, DataPort: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := seeder.Start(); err != nil {
		t.Fatal(err)
	}
	defer seeder.Stop()
	meta, err := seeder.AddSeed(src, 1) // 1-byte pieces -> one hash per byte
	if err != nil {
		t.Fatal(err)
	}
	if meta.NumPieces() != pieces {
		t.Fatalf("expected %d pieces, got %d", pieces, meta.NumPieces())
	}
	// Preconditions that make this a genuine regression test: the metadata frame
	// must exceed the piece-frame cap (the old limit) yet stay under the metadata
	// cap, otherwise the test would not have failed before the fix.
	ser, err := meta.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if uint32(len(ser)) <= protocol.DefaultMaxFrame {
		t.Fatalf("test not exercising the bug: meta size %d <= DefaultMaxFrame %d", len(ser), protocol.DefaultMaxFrame)
	}
	if uint32(len(ser)) > protocol.MetaMaxFrame {
		t.Fatalf("test meta size %d exceeds MetaMaxFrame %d", len(ser), protocol.MetaMaxFrame)
	}

	dl, err := NewNode(Options{DiscoveryPort: 51461, DataPort: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := dl.Start(); err != nil {
		t.Fatal(err)
	}
	defer dl.Stop()

	// Dial the seeder's TCP listener on loopback (the advertised host is the
	// machine's primary outbound IP, which may not accept loopback dials on every
	// host; the port is what matters for reaching this node's server).
	_, port, err := net.SplitHostPort(seeder.MyAddr())
	if err != nil {
		t.Fatalf("split seeder addr %q: %v", seeder.MyAddr(), err)
	}
	ih := meta.InfoHash()
	got, err := dl.GetMeta(net.JoinHostPort("127.0.0.1", port), ih)
	if err != nil {
		t.Fatalf("GetMeta with oversized metadata failed: %v", err)
	}
	if got.InfoHashHex() != meta.InfoHashHex() {
		t.Fatalf("info hash mismatch after GetMeta: %s vs %s", got.InfoHashHex(), meta.InfoHashHex())
	}
	if got.NumPieces() != pieces {
		t.Fatalf("piece count after GetMeta = %d, want %d", got.NumPieces(), pieces)
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
