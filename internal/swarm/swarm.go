// Package swarm ties discovery and transport together into a running node. It
// serves files over TCP to other peers, connects out to discovered peers,
// downloads missing pieces (verifying each one), and keeps sharing pieces it
// already has so that every downloader becomes a seeder.
package swarm

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fredchuanshu/chuanshu/internal/discovery"
	"github.com/fredchuanshu/chuanshu/internal/protocol"
	"github.com/fredchuanshu/chuanshu/internal/store"
)

// Status describes the state of one shared file in this node.
type Status struct {
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

// file holds the runtime state for one shared file.
type file struct {
	meta  *protocol.Metadata
	store *store.Store
	have  *protocol.Bitfield
	// wanted tracks pieces still being fetched to avoid duplicate requests.
	mu    sync.Mutex
	peers map[string]bool
	dl    int64
	ul    int64
}

func (f *file) addDL(n int64) { atomic.AddInt64(&f.dl, n) }
func (f *file) addUL(n int64) { atomic.AddInt64(&f.ul, n) }

// Node is a single running chuanshu instance: a TCP server, a discovery service
// and the set of files it is sharing or downloading.
type Node struct {
	tcpAddr  string
	dataPort int
	peerID   []byte
	registry *discovery.Registry
	disc     *discovery.Service

	mu    sync.RWMutex
	files map[string]*file

	listener net.Listener
	stop     chan struct{}
	wg       sync.WaitGroup
	log      *log.Logger

	// onProgress is invoked whenever any file's progress changes (may be nil).
	onProgress func(Status)
}

// Options configure a Node.
type Options struct {
	// DiscoveryPort is the shared UDP multicast port for peer discovery. Zero
	// uses the well-known default.
	DiscoveryPort int
	// DataPort is the TCP port the peer serves pieces on. Zero lets the OS pick
	// an ephemeral free port (so multiple instances can run on one host).
	DataPort int
	Log      *log.Logger
	// ReservePeerID overrides the random peer id (tests).
	ReservePeerID []byte
	OnProgress    func(Status)
}

// NewNode creates (but does not start) a node.
func NewNode(opts Options) (*Node, error) {
	if opts.DiscoveryPort == 0 {
		opts.DiscoveryPort = discovery.DiscoveryUDPPort
	}
	if opts.Log == nil {
		opts.Log = log.New(io.Discard, "", 0)
	}
	id := make([]byte, 8)
	if len(opts.ReservePeerID) == 8 {
		copy(id, opts.ReservePeerID)
	} else {
		rand.Read(id)
	}
	n := &Node{
		dataPort:   opts.DataPort,
		peerID:     id,
		files:      make(map[string]*file),
		stop:       make(chan struct{}),
		log:        opts.Log,
		onProgress: opts.OnProgress,
	}
	// The discovery service advertises a TCP port that is only known after the
	// listener is opened, so we wire the real TCP port into discovery at Start.
	n.disc = discovery.NewService(opts.DiscoveryPort, 0)
	n.registry = n.disc.Registry()
	return n, nil
}

// Start begins discovery and the TCP server.
func (n *Node) Start() error {
	portSpec := "0"
	if n.dataPort != 0 {
		portSpec = fmt.Sprintf("%d", n.dataPort)
	}
	ln, err := net.Listen("tcp", "0.0.0.0:"+portSpec)
	if err != nil {
		return fmt.Errorf("tcp listen %s: %w", portSpec, err)
	}
	n.listener = ln
	host, _ := localAddr()
	if _, port, err := net.SplitHostPort(ln.Addr().String()); err == nil {
		n.dataPort, _ = strconv.Atoi(port)
		n.tcpAddr = net.JoinHostPort(host, port)
	}
	// Tell discovery which TCP port we are actually serving on so peers can
	// connect, then start the discovery service.
	n.disc.SetDataPort(n.dataPort)
	if err := n.disc.Start(); err != nil {
		ln.Close()
		return fmt.Errorf("discovery start: %w", err)
	}
	n.wg.Add(1)
	go n.acceptLoop()
	n.wg.Add(1)
	go n.connectLoop()
	n.wg.Add(1)
	go n.publishLoop()
	return nil
}

// Stop shuts the node down.
func (n *Node) Stop() {
	select {
	case <-n.stop:
		return
	default:
		close(n.stop)
	}
	if n.listener != nil {
		n.listener.Close()
	}
	n.disc.Stop()
	n.wg.Wait()
}

// MyAddr returns this node's advertised TCP address (host:port).
func (n *Node) MyAddr() string { return n.tcpAddr }

// AddSeed registers an already-complete source file to share (and seed).
func (n *Node) AddSeed(path string, pieceLen int) (*protocol.Metadata, error) {
	meta, _, err := store.BuildMetadata(path, pieceLen)
	if err != nil {
		return nil, err
	}
	st, err := store.OpenExisting(path, meta)
	if err != nil {
		return nil, err
	}
	key := meta.InfoHashHex()
	n.mu.Lock()
	if _, ok := n.files[key]; ok {
		n.mu.Unlock()
		return meta, nil
	}
	n.files[key] = &file{
		meta:  meta,
		store: st,
		have:  allSet(meta.NumPieces()),
		peers: make(map[string]bool),
	}
	n.mu.Unlock()
	n.refreshAdverts()
	return meta, nil
}

// StartDownload registers an incoming download described by meta into dir. If a
// matching partial already exists, its completed pieces are reused.
func (n *Node) StartDownload(dir string, meta *protocol.Metadata) error {
	key := meta.InfoHashHex()
	n.mu.Lock()
	if _, ok := n.files[key]; ok {
		n.mu.Unlock()
		return nil
	}
	n.mu.Unlock()

	st, err := store.CreateDownload(dir, meta)
	if err != nil {
		return err
	}
	have := protocol.NewBitfield(meta.NumPieces())
	for i := 0; i < meta.NumPieces(); i++ {
		if st.HasPiece(i) {
			have.Set(i)
		}
	}
	n.mu.Lock()
	n.files[key] = &file{
		meta:  meta,
		store: st,
		have:  have,
		peers: make(map[string]bool),
	}
	n.mu.Unlock()
	n.refreshAdverts()
	return nil
}

// DiscoveredHashes returns the info hashes of all files advertised by any peer
// on the network, deduplicated.
func (n *Node) DiscoveredHashes() []string {
	return n.registry.Hashes(15 * time.Second)
}

// SeederCount returns how many known peers currently hold a complete copy of
// the given file.
func (n *Node) SeederCount(infoHashHex string) int {
	return n.registry.Seeders(infoHashHex, 15*time.Second)
}

// FindPeer returns any known peer address advertising the given info hash, or
// the empty string if none is known yet. Used to bootstrap metadata retrieval.
func (n *Node) FindPeer(infoHashHex string) string {
	raw, err := hex.DecodeString(infoHashHex)
	if err != nil || len(raw) != 32 {
		return ""
	}
	var ih [32]byte
	copy(ih[:], raw)
	peers := n.registry.Peers(ih, n.tcpAddr, 15*time.Second)
	if len(peers) == 0 {
		return ""
	}
	return peers[0].TCPAddr
}

// AbortDownload removes an in-progress download and deletes its partial file.
func (n *Node) AbortDownload(infoHashHex string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	f, ok := n.files[infoHashHex]
	if !ok || f.have.Complete() {
		return
	}
	f.store.DeletePartial()
	delete(n.files, infoHashHex)
}

// StatusFor returns a snapshot of a single file's status.
func (n *Node) StatusFor(infoHashHex string) (Status, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	f, ok := n.files[infoHashHex]
	if !ok {
		return Status{}, false
	}
	return n.statusLocked(f), true
}

// AllStatus returns snapshots of every shared file.
func (n *Node) AllStatus() []Status {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make([]Status, 0, len(n.files))
	for _, f := range n.files {
		out = append(out, n.statusLocked(f))
	}
	return out
}

func (n *Node) statusLocked(f *file) Status {
	nTotal := f.meta.NumPieces()
	have := f.have.Count()
	prog := 0.0
	if nTotal > 0 {
		prog = float64(have) / float64(nTotal)
	}
	peers := len(n.registry.Peers(f.meta.InfoHash(), n.tcpAddr, 15*time.Second))
	return Status{
		InfoHash:   f.meta.InfoHashHex(),
		Name:       f.meta.Name,
		Length:     f.meta.Length,
		Pieces:     nTotal,
		HavePieces: have,
		Complete:   have == nTotal,
		Progress:   prog,
		Peers:      peers,
		Downloaded: atomic.LoadInt64(&f.dl),
		Uploaded:   atomic.LoadInt64(&f.ul),
	}
}

// GetMeta retrieves a file's metadata from a known peer address. Used by a
// download that has only the info hash to obtain the piece list.
func (n *Node) GetMeta(addr string, infoHash [32]byte) (*protocol.Metadata, error) {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := n.sendHello(conn, infoHash); err != nil {
		return nil, err
	}
	if err := protocol.WriteFrame(conn, protocol.MsgGetMeta, nil); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	t, payload, err := protocol.ReadFrame(conn)
	if err != nil {
		return nil, err
	}
	if t != protocol.MsgMeta {
		return nil, protocol.ErrUnknownInfo
	}
	var meta protocol.Metadata
	if err := json.Unmarshal(payload, &meta); err != nil {
		return nil, err
	}
	if err := meta.Verify(); err != nil {
		return nil, err
	}
	// Confirm the metadata actually hashes to the info hash we asked for.
	if meta.InfoHash() != infoHash {
		return nil, protocol.ErrHashMismatch
	}
	return &meta, nil
}

// ---- advertising ----

func (n *Node) refreshAdverts() {
	n.mu.RLock()
	adverts := make([]discovery.Advert, 0, len(n.files))
	for _, f := range n.files {
		adverts = append(adverts, discovery.Advert{
			InfoHash: f.meta.InfoHash(),
			Complete: f.have.Complete(),
		})
	}
	n.mu.RUnlock()
	n.disc.SetAdvertised(adverts)
}

func (n *Node) publishLoop() {
	defer n.wg.Done()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-t.C:
			n.refreshAdverts()
		}
	}
}

// ---- connecting to peers ----

func (n *Node) connectLoop() {
	defer n.wg.Done()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-t.C:
			n.dialNeededPeers()
		}
	}
}

// dialNeededPeers opens outgoing connections to peers for files we still need
// pieces from, then runs a download attempt on each connection.
func (n *Node) dialNeededPeers() {
	n.mu.RLock()
	type target struct {
		file *file
		addr string
	}
	var targets []target
	for _, f := range n.files {
		if f.have.Complete() {
			continue
		}
		peers := n.registry.Peers(f.meta.InfoHash(), n.tcpAddr, 15*time.Second)
		for _, p := range peers {
			f.mu.Lock()
			known := f.peers[p.TCPAddr]
			f.mu.Unlock()
			if known {
				continue
			}
			targets = append(targets, target{file: f, addr: p.TCPAddr})
		}
	}
	n.mu.RUnlock()

	for _, t := range targets {
		t.file.mu.Lock()
		t.file.peers[t.addr] = true
		t.file.mu.Unlock()
		n.wg.Add(1)
		go func(f *file, addr string) {
			defer n.wg.Done()
			n.workWithPeer(f, addr)
		}(t.file, t.addr)
	}
}

// ---- TCP server ----

func (n *Node) acceptLoop() {
	defer n.wg.Done()
	for {
		conn, err := n.listener.Accept()
		if err != nil {
			select {
			case <-n.stop:
				return
			default:
				continue
			}
		}
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			n.serveConn(conn)
		}()
	}
}

// serveConn handles one incoming peer connection.
func (n *Node) serveConn(conn net.Conn) {
	defer conn.Close()
	// Read the client's hello to learn which info hash it wants.
	metaInfo, err := n.readHello(conn)
	if err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	n.mu.RLock()
	f, ok := n.files[metaInfo]
	n.mu.RUnlock()
	if !ok {
		// Unknown to us; maybe it just wants metadata for a file we don't have.
		n.serveUnknown(conn)
		return
	}
	n.serveFile(conn, f)
}

// serveUnknown handles a connection from a peer asking for a file we don't have
// (we just reject; a node only serves files it is sharing).
func (n *Node) serveUnknown(conn net.Conn) {
	// Drain one request frame if present, then reject.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	t, _, err := protocol.ReadFrame(conn)
	if err != nil {
		return
	}
	if t == protocol.MsgGetMeta {
		_ = protocol.WriteFrame(conn, protocol.MsgReject, nil)
	}
}

// serveFile serves metadata and pieces for a shared file to a single peer.
func (n *Node) serveFile(conn net.Conn, f *file) {
	for {
		select {
		case <-n.stop:
			return
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		t, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			return
		}
		switch t {
		case protocol.MsgGetMeta:
			b, err := json.Marshal(f.meta)
			if err != nil {
				return
			}
			if err := protocol.WriteFrame(conn, protocol.MsgMeta, b); err != nil {
				return
			}
		case protocol.MsgRequest:
			idx, err := protocol.DecodeIndex(payload)
			if err != nil {
				return
			}
			if !f.have.Has(idx) {
				_ = protocol.WriteFrame(conn, protocol.MsgReject, protocol.EncodeIndex(idx))
				continue
			}
			data, err := f.store.ReadPiece(idx)
			if err != nil {
				_ = protocol.WriteFrame(conn, protocol.MsgReject, protocol.EncodeIndex(idx))
				continue
			}
			if err := protocol.WriteFrame(conn, protocol.MsgPiece, protocol.EncodePieceResp(idx, data)); err != nil {
				return
			}
			f.addUL(int64(len(data)))
			n.notify(f)
		case protocol.MsgHello:
			// Ignore duplicate hellos.
		default:
			return
		}
	}
}

// ---- connecting out: download from a peer ----

// workWithPeer downloads needed pieces from a single peer, then closes the
// connection. Bad peers (hash mismatches) are abandoned.
func (n *Node) workWithPeer(f *file, addr string) {
	defer func() {
		f.mu.Lock()
		delete(f.peers, addr)
		f.mu.Unlock()
	}()

	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return
	}
	defer conn.Close()

	if err := n.sendHello(conn, f.meta.InfoHash()); err != nil {
		return
	}
	// We assume a peer could serve any piece and rely on it replying with a
	// reject for pieces it lacks. This keeps the wire protocol minimal while a
	// node that has only part of a file still contributes the pieces it holds.
	remoteHave := allSet(f.meta.NumPieces())

	for {
		select {
		case <-n.stop:
			return
		default:
		}
		missing := f.have.Missing()
		if len(missing) == 0 {
			return
		}
		// Request a batch of pieces we still lack and the peer has.
		batch := chooseBatch(missing, remoteHave, 4)
		if len(batch) == 0 {
			return // peer has nothing useful right now
		}
		for _, idx := range batch {
			if err := protocol.WriteFrame(conn, protocol.MsgRequest, protocol.EncodeIndex(idx)); err != nil {
				return
			}
		}
		got := 0
		_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		for got < len(batch) {
			t, payload, err := protocol.ReadFrame(conn)
			if err != nil {
				return
			}
			switch t {
			case protocol.MsgPiece:
				idx, data, err := protocol.DecodePieceResp(payload)
				if err != nil {
					return
				}
				got++
				if f.have.Has(idx) {
					continue // already have it (another peer won the race)
				}
				if err := f.store.WritePiece(idx, data); err != nil {
					// Corrupt piece: drop the peer.
					n.log.Printf("dropping peer %s: %v", addr, err)
					return
				}
				f.have.Set(idx)
				f.addDL(int64(len(data)))
				n.notify(f)
				if f.have.Complete() {
					n.finalizeIfComplete(f)
					return
				}
			case protocol.MsgReject:
				got++
			default:
				return
			}
		}
	}
}

// ---- handshake helpers ----

func (n *Node) sendHello(conn net.Conn, infoHash [32]byte) error {
	h := protocol.Hello{Version: protocol.Version, PeerID: n.peerID, InfoHash: infoHash[:]}
	b, _ := json.Marshal(h)
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return protocol.WriteFrame(conn, protocol.MsgHello, b)
}

// readHello reads and validates a hello frame, returning the requested info
// hash.
func (n *Node) readHello(conn net.Conn) (string, error) {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	t, payload, err := protocol.ReadFrame(conn)
	if err != nil {
		return "", err
	}
	if t != protocol.MsgHello {
		return "", protocol.ErrBadFrame
	}
	var h protocol.Hello
	if err := json.Unmarshal(payload, &h); err != nil {
		return "", err
	}
	if h.Version != protocol.Version {
		return "", protocol.ErrVersion
	}
	if len(h.InfoHash) != 32 {
		return "", protocol.ErrBadFrame
	}
	return hex.EncodeToString(h.InfoHash), nil
}

func allSet(n int) *protocol.Bitfield {
	bf := protocol.NewBitfield(n)
	for i := 0; i < n; i++ {
		bf.Set(i)
	}
	return bf
}

// chooseBatch picks up to max piece indices from missing that remoteHave also
// has.
func chooseBatch(missing []int, remoteHave *protocol.Bitfield, max int) []int {
	var out []int
	for _, idx := range missing {
		if remoteHave.Has(idx) {
			out = append(out, idx)
			if len(out) >= max {
				break
			}
		}
	}
	return out
}

// notify emits the current status to the progress callback if set.
func (n *Node) notify(f *file) {
	if n.onProgress == nil {
		return
	}
	n.mu.RLock()
	s := n.statusLocked(f)
	n.mu.RUnlock()
	n.onProgress(s)
}

// finalizeIfComplete renames the partial file to its final name once complete.
func (n *Node) finalizeIfComplete(f *file) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !f.have.Complete() {
		return
	}
	if err := f.store.Finalize(); err != nil {
		n.log.Printf("finalize %s: %v", f.meta.Name, err)
		return
	}
	n.refreshAdvertsLocked()
	n.log.Printf("completed download: %s -> %s", f.meta.Name, f.store.Target)
}

// refreshAdvertsLocked recomputes advert flags while the write lock is held.
func (n *Node) refreshAdvertsLocked() {
	adverts := make([]discovery.Advert, 0, len(n.files))
	for _, f := range n.files {
		adverts = append(adverts, discovery.Advert{
			InfoHash: f.meta.InfoHash(),
			Complete: f.have.Complete(),
		})
	}
	n.disc.SetAdvertised(adverts)
}

// WaitComplete blocks until the given file is complete or the node stops.
func (n *Node) WaitComplete(infoHashHex string) {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		n.mu.RLock()
		f, ok := n.files[infoHashHex]
		var done bool
		if ok {
			done = f.have.Complete()
		} else {
			done = true // no such file registered; stop waiting
		}
		n.mu.RUnlock()
		if done {
			return
		}
		select {
		case <-n.stop:
			return
		case <-t.C:
		}
	}
}
