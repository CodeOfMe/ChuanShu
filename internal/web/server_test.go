package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CodeOfMe/ChuanShu/internal/swarm"
)

// startNode spins up a swarm node + web server bound to loopback for tests.
func startNode(t *testing.T) *Server {
	t.Helper()
	n, err := swarm.NewNode(swarm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Stop)
	// Bind an ephemeral loopback port. Port 0 lets the OS choose.
	srv, err := NewServer(n, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

func baseURL(s *Server) string { return "http://" + s.Addr() }

func TestServerRejectsNonLoopback(t *testing.T) {
	n, _ := swarm.NewNode(swarm.Options{})
	if _, err := NewServer(n, "0.0.0.0:8080"); err == nil {
		t.Fatal("expected NewServer to refuse a non-loopback bind address")
	}
}

func TestIndexServed(t *testing.T) {
	s := startNode(t)
	resp, err := http.Get(baseURL(s) + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct == "" {
		t.Fatal("missing Content-Type")
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if !bytes.Contains(buf.Bytes(), []byte("传书")) {
		t.Fatal("index did not contain expected title")
	}
}

func TestShareAndStatus(t *testing.T) {
	s := startNode(t)
	src := filepath.Join(t.TempDir(), "sample.bin")
	if err := os.WriteFile(src, bytes.Repeat([]byte("x"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"path": src})
	resp, err := http.Post(baseURL(s)+"/api/share", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("share status = %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["infoHash"] == "" || out["infoHash"] == nil {
		t.Fatalf("no infoHash in response: %v", out)
	}

	// status should now list one complete file.
	resp2, err := http.Get(baseURL(s) + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var st struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if len(st.Files) != 1 || st.Files[0]["complete"] != true {
		t.Fatalf("expected 1 complete file, got %v", st.Files)
	}
}

func TestShareRejectsMissingFile(t *testing.T) {
	s := startNode(t)
	body, _ := json.Marshal(map[string]any{"path": "/definitely/not/here.bin"})
	resp, err := http.Post(baseURL(s)+"/api/share", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestGetRejectsBadHash(t *testing.T) {
	s := startNode(t)
	body, _ := json.Marshal(map[string]any{"hash": "nothex"})
	resp, err := http.Post(baseURL(s)+"/api/get", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestGetNoSeeder(t *testing.T) {
	s := startNode(t)
	// A syntactically valid but unknown hash should 404 (no seeder).
	body, _ := json.Marshal(map[string]any{"hash": "0000000000000000000000000000000000000000000000000000000000000000"})
	resp, err := http.Post(baseURL(s)+"/api/get", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 404 (no peer found) or 502 (peer refused); either is acceptable here.
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 404/502", resp.StatusCode)
	}
}

func TestMethodsRestricted(t *testing.T) {
	s := startNode(t)
	// GET on /api/share must fail; it is POST-only.
	resp, err := http.Get(baseURL(s) + "/api/share")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/share = %d, want 405", resp.StatusCode)
	}
}

func TestEndToEndShareGetStatus(t *testing.T) {
	// Two independent web+swarm nodes on loopback. Node A shares, node B is
	// asked to download via its HTTP API; poll B's status until complete.
	a := startNode(t)
	b := startNode(t)
	// Give peers time to discover each other on the multicast group.
	time.Sleep(2500 * time.Millisecond)

	src := filepath.Join(t.TempDir(), "hello.bin")
	data := bytes.Repeat([]byte("hi"), 100_000)
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	shareBody, _ := json.Marshal(map[string]any{"path": src})
	resp, err := http.Post(baseURL(a)+"/api/share", "application/json", bytes.NewReader(shareBody))
	if err != nil {
		t.Fatal(err)
	}
	var shareOut map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&shareOut)
	resp.Body.Close()
	infoHash, _ := shareOut["infoHash"].(string)
	if infoHash == "" {
		t.Fatal("no infoHash from A's share")
	}
	// Give discovery on B some time to hear A's announce.
	deadline := time.Now().Add(10 * time.Second)
	var dlOut struct {
		InfoHash string `json:"infoHash"`
		Name     string `json:"name"`
	}
	for time.Now().Before(deadline) {
		getBody, _ := json.Marshal(map[string]any{"hash": infoHash, "out": t.TempDir()})
		r, err := http.Post(baseURL(b)+"/api/get", "application/json", bytes.NewReader(getBody))
		if err == nil && r.StatusCode == 200 {
			_ = json.NewDecoder(r.Body).Decode(&dlOut)
			r.Body.Close()
			if dlOut.InfoHash == infoHash {
				break
			}
		} else if r != nil {
			r.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	if dlOut.InfoHash != infoHash {
		t.Fatalf("B never accepted the download (last=%+v)", dlOut)
	}
	// Poll B's /api/status until the file is complete or timeout.
	deadline = time.Now().Add(15 * time.Second)
	var done bool
	for time.Now().Before(deadline) && !done {
		r, err := http.Get(baseURL(b) + "/api/status")
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			Files []map[string]any `json:"files"`
		}
		_ = json.NewDecoder(r.Body).Decode(&st)
		r.Body.Close()
		for _, f := range st.Files {
			if f["infoHash"] == infoHash && f["complete"] == true {
				done = true
				break
			}
		}
		if !done {
			time.Sleep(400 * time.Millisecond)
		}
	}
	if !done {
		t.Fatal("B never reported completion")
	}
}
