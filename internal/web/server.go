// Package web implements a minimal local web control plane over a running
// swarm node so the tool can be driven from a browser instead of the terminal.
// It should only be bound to loopback: peers on the LAN reach the node directly
// via TCP/UDP, not through this server.
package web

import (
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/CodeOfMe/ChuanShu/internal/swarm"
)

//go:embed index.html
var indexHTML []byte

// Server exposes a running swarm node over HTTP on a loopback interface.
//
// Routes:
//
//	GET  /              -- the single-page UI
//	GET  /api/discover  -- files advertised by other LAN peers
//	GET  /api/status    -- status of every file this node holds
//	POST /api/share     -- begin seeding a local file
//	POST /api/get       -- begin downloading a file by info hash
//
// Server is safe for concurrent use.
type Server struct {
	node     *swarm.Node
	addr     string
	listener net.Listener
	http     *http.Server
	mu       sync.Mutex
	closed   bool
}

// NewServer prepares a web control plane over n bound to addr (which must be
// on the loopback interface).
func NewServer(n *swarm.Node, addr string) (*Server, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("split addr %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("web UI must bind a loopback address, got %q", addr)
	}
	mux := http.NewServeMux()
	s := &Server{node: n, addr: addr}
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/discover", s.handleDiscover)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/share", s.handleShare)
	mux.HandleFunc("/api/get", s.handleGet)
	s.http = &http.Server{Handler: mux}
	return s, nil
}

// Start begins serving on a fresh listener. It returns once the listener is
// open; the HTTP serve loop runs in the background.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.addr, err)
	}
	s.listener = ln
	go func() {
		_ = s.http.Serve(ln)
	}()
	return nil
}

// Addr returns the resolved listen address (host:port). Valid after Start.
func (s *Server) Addr() string {
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.addr
}

// Close stops the server. Safe to call more than once.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.http.Close()
}

// --- index ---

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

// --- discovery ---

// discoveredEntry is one row in the discover listing.
type discoveredEntry struct {
	Hash    string `json:"hash"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Pieces  int    `json:"pieces"`
	Seeders int    `json:"seeders"`
}

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	hashes := s.node.DiscoveredHashes()
	entries := make([]discoveredEntry, len(hashes))
	// Look up metadata for each hash in parallel with a small worker pool and
	// per-hash timeout so a slow or dead peer cannot hang the whole response.
	const workers = 4
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, h := range hashes {
		entries[i] = discoveredEntry{Hash: h, Name: "(未知)", Seeders: s.node.SeederCount(h)}
		addr := s.node.FindPeer(h)
		if addr == "" {
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, h, addr string) {
			defer wg.Done()
			defer func() { <-sem }()
			ih, err := hexToHash(h)
			if err != nil {
				return
			}
			meta, err := s.node.GetMeta(addr, ih)
			if err != nil || meta == nil {
				return
			}
			entries[i].Name = meta.Name
			entries[i].Size = meta.Length
			entries[i].Pieces = meta.NumPieces()
		}(i, h, addr)
	}
	wg.Wait()
	writeJSON(w, map[string]any{"files": entries})
}

// --- status ---

// statusView mirrors the subset of swarm.Status the UI needs.
type statusView struct {
	InfoHash   string  `json:"infoHash"`
	Name       string  `json:"name"`
	Length     int64   `json:"length"`
	Pieces     int     `json:"pieces"`
	HavePieces int     `json:"havePieces"`
	Complete   bool    `json:"complete"`
	Progress   float64 `json:"progress"`
	Peers      int     `json:"peers"`
	Downloaded int64   `json:"downloaded"`
	Uploaded   int64   `json:"uploaded"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	statuses := s.node.AllStatus()
	views := make([]statusView, 0, len(statuses))
	for _, st := range statuses {
		views = append(views, statusView(st))
	}
	writeJSON(w, map[string]any{"files": views})
}

// --- share ---

type shareRequest struct {
	Path     string `json:"path"`
	PieceKiB int    `json:"pieceKiB"`
}

type shareResponse struct {
	InfoHash string `json:"infoHash"`
	Name     string `json:"name"`
	Length   int64  `json:"length"`
	Pieces   int    `json:"pieces"`
	PieceLen int    `json:"pieceLen"`
}

func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req shareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	path, err := expandHome(strings.TrimSpace(req.Path))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("file not accessible: %v", err))
		return
	}
	pieceKiB := req.PieceKiB
	if pieceKiB <= 0 {
		pieceKiB = 256
	}
	meta, err := s.node.AddSeed(path, pieceKiB*1024)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, shareResponse{
		InfoHash: meta.InfoHashHex(),
		Name:     meta.Name,
		Length:   meta.Length,
		Pieces:   meta.NumPieces(),
		PieceLen: meta.PieceLength,
	})
}

// --- download ---

type getRequest struct {
	Hash string `json:"hash"`
	Out  string `json:"out"`
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req getRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ih, err := hexToHash(strings.TrimSpace(req.Hash))
	if err != nil {
		writeError(w, http.StatusBadRequest, "hash must be 64 hex characters")
		return
	}
	out := strings.TrimSpace(req.Out)
	if out == "" {
		out = "."
	}
	out, err = expandHome(out)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hashHex := fmtBytes(ih[:])
	addr := s.node.FindPeer(hashHex)
	if addr == "" {
		writeError(w, http.StatusNotFound, "no seeder advertising this hash on the LAN yet")
		return
	}
	meta, err := s.node.GetMeta(addr, ih)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("peer %s refused metadata: %v", addr, err))
		return
	}
	if err := s.node.StartDownload(out, meta); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"infoHash": hashHex, "name": meta.Name, "length": meta.Length})
}

// --- helpers ---

func hexToHash(h string) ([32]byte, error) {
	var out [32]byte
	raw, err := hex.DecodeString(h)
	if err != nil || len(raw) != 32 {
		return out, fmt.Errorf("invalid info hash")
	}
	copy(out[:], raw)
	return out, nil
}

// expandHome rewrites a leading "~" to the current user's home directory. It
// only does this for the exact prefix "~" or "~/" to avoid surprises.
func expandHome(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}

// fmtBytes is a tiny local helper to hex-encode a byte slice without an extra
// import in this file.
func fmtBytes(b []byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexDigits[c>>4]
		out[i*2+1] = hexDigits[c&0x0f]
	}
	return string(out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
