// Package discovery implements zero-config peer discovery on a local network.
// There is no tracker: each peer periodically broadcasts its advertised files
// (info hash, TCP port, completeness) to the LAN and also replies directly to
// anyone who announces, so a newcomer quickly learns every peer interested in
// the same files.
package discovery

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// DiscoveryUDPPort is the well-known UDP port used for LAN peer discovery. It is
// shared by all peers (bound with SO_REUSEPORT) and independent of each peer's
// TCP data port, which is carried inside the discovery packets themselves.
const DiscoveryUDPPort = 51218

const (
	magic = 0x43485531 // "CHU1"
)

// multicastGroup is the administratively-scoped IPv4 multicast group used for
// discovery. Multicast (rather than broadcast) delivers a copy of each announce
// to every joined peer, including peers on the same host.
var multicastGroup = net.IPv4(239, 77, 84, 1)

// Packet types.
const (
	typeAnnounce byte = 1
	typeReply    byte = 2
)

// FlagComplete marks a peer that holds a complete copy of a file.
const FlagComplete byte = 1 << 0

// ErrBadPacket indicates a malformed discovery packet.
var ErrBadPacket = errors.New("chuanshu: bad discovery packet")

// Advert describes one file this peer is offering to the swarm.
type Advert struct {
	InfoHash [32]byte
	Complete bool
}

// Peer is a discovered peer.
type Peer struct {
	TCPAddr  string
	InfoHash [32]byte
	Complete bool
	LastSeen time.Time
}

// Registry stores discovered peers, keyed by info hash then peer address.
type Registry struct {
	mu    sync.Mutex
	peers map[string]map[string]*Peer
}

// NewRegistry returns an empty peer registry.
func NewRegistry() *Registry {
	return &Registry{peers: make(map[string]map[string]*Peer)}
}

func (r *Registry) add(infoHashHex string, p *Peer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.peers[infoHashHex]
	if !ok {
		m = make(map[string]*Peer)
		r.peers[infoHashHex] = m
	}
	existing, ok := m[p.TCPAddr]
	if !ok {
		m[p.TCPAddr] = p
		return
	}
	existing.LastSeen = p.LastSeen
	existing.Complete = existing.Complete || p.Complete
}

// Peers returns the currently-known peers for an info hash, excluding self and
// any that have not been seen within maxAge.
func (r *Registry) Peers(infoHash [32]byte, self string, maxAge time.Duration) []Peer {
	key := hex.EncodeToString(infoHash[:])
	cutoff := time.Now().Add(-maxAge)
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.peers[key]
	out := make([]Peer, 0, len(m))
	for _, p := range m {
		if p.LastSeen.Before(cutoff) {
			continue
		}
		if p.TCPAddr == self {
			continue
		}
		out = append(out, *p)
	}
	return out
}

// Prune drops stale entries older than maxAge.
func (r *Registry) Prune(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	r.mu.Lock()
	defer r.mu.Unlock()
	for h, m := range r.peers {
		for addr, p := range m {
			if p.LastSeen.Before(cutoff) {
				delete(m, addr)
			}
		}
		if len(m) == 0 {
			delete(r.peers, h)
		}
	}
}

// Hashes returns the hex info hashes of all known files that have at least one
// peer seen within maxAge.
func (r *Registry) Hashes(maxAge time.Duration) []string {
	cutoff := time.Now().Add(-maxAge)
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for h, m := range r.peers {
		for _, p := range m {
			if !p.LastSeen.Before(cutoff) {
				out = append(out, h)
				break
			}
		}
	}
	return out
}

// Seeders counts how many peers currently advertise a complete copy of a file.
func (r *Registry) Seeders(infoHash string, maxAge time.Duration) int {
	cutoff := time.Now().Add(-maxAge)
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.peers[infoHash]
	c := 0
	for _, p := range m {
		if !p.LastSeen.Before(cutoff) && p.Complete {
			c++
		}
	}
	return c
}

// Service is the UDP discovery listener/announcer.
type Service struct {
	port     int
	tcpPort  int
	interval time.Duration

	reg  *Registry
	mu   sync.Mutex
	adv  map[[32]byte]Advert
	conn *net.UDPConn

	stop   chan struct{}
	closed chan struct{}
}

// NewService creates a discovery service listening on udpPort and advertising a
// TCP file server on tcpPort.
func NewService(udpPort, tcpPort int) *Service {
	return &Service{
		port:     udpPort,
		tcpPort:  tcpPort,
		interval: 3 * time.Second,
		reg:      NewRegistry(),
		adv:      make(map[[32]byte]Advert),
		stop:     make(chan struct{}),
		closed:   make(chan struct{}),
	}
}

// Registry exposes the shared peer registry.
func (s *Service) Registry() *Registry { return s.reg }

// SetDataPort sets the TCP port advertised to peers. It must be called before
// Start so the first announce already carries the real port.
func (s *Service) SetDataPort(port int) {
	s.mu.Lock()
	s.tcpPort = port
	s.mu.Unlock()
}

// SetAdvertised replaces the set of files this peer advertises to the LAN.
func (s *Service) SetAdvertised(adverts []Advert) {
	m := make(map[[32]byte]Advert, len(adverts))
	for _, a := range adverts {
		m[a.InfoHash] = a
	}
	s.mu.Lock()
	s.adv = m
	s.mu.Unlock()
}

func (s *Service) advertisedSnapshot() []Advert {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Advert, 0, len(s.adv))
	for _, a := range s.adv {
		out = append(out, a)
	}
	return out
}

// Start binds the UDP multicast socket and begins announcing/listening.
func (s *Service) Start() error {
	lc := net.ListenConfig{
		Control: setMulticastSocketOptions,
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", s.port))
	if err != nil {
		return fmt.Errorf("listen udp %d: %w", s.port, err)
	}
	conn := pc.(*net.UDPConn)
	s.conn = conn
	if err := joinMulticast(conn); err != nil {
		conn.Close()
		return fmt.Errorf("join multicast: %w", err)
	}
	go s.readLoop()
	go s.announceLoop()
	go s.pruneLoop()
	return nil
}

// setMulticastSocketOptions and joinMulticast are defined in build-tagged
// files: multicast_unix.go (using raw setsockopt via golang.org/x/sys/unix)
// and multicast_other.go (portable net.PacketConn fallback for Windows/plan9).

// Stop shuts the service down.
func (s *Service) Stop() {
	close(s.stop)
	<-s.closed
}

func (s *Service) pruneLoop() {
	defer func() { close(s.closed) }()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			s.conn.Close()
			return
		case <-t.C:
			s.reg.Prune(15 * time.Second)
		}
	}
}

func (s *Service) announceLoop() {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	// Announce once immediately so newcomers that already announced get a reply
	// path and we do not wait a full interval to be heard.
	s.broadcastAll()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.broadcastAll()
		}
	}
}

func (s *Service) broadcastAll() {
	adverts := s.advertisedSnapshot()
	if len(adverts) == 0 {
		return
	}
	pkt := s.encode(typeAnnounce, adverts)
	if pkt == nil {
		return
	}
	group := &net.UDPAddr{IP: multicastGroup, Port: s.port}
	_, _ = s.conn.WriteTo(pkt, group)
}

func (s *Service) readLoop() {
	buf := make([]byte, 2048)
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, remote, err := s.conn.ReadFrom(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		pkt := append([]byte(nil), buf[:n]...)
		s.handle(pkt, remote)
	}
}

func (s *Service) handle(pkt []byte, remote net.Addr) {
	pType, adverts, err := decode(pkt)
	if err != nil {
		return
	}
	now := time.Now()
	for _, a := range adverts {
		s.reg.add(hex.EncodeToString(a.InfoHash[:]), &Peer{
			TCPAddr:  a.tcpAddr(remote),
			InfoHash: a.InfoHash,
			Complete: a.Complete,
			LastSeen: now,
		})
	}
	// Reply directly with our own adverts so the sender learns our peers fast.
	if pType == typeAnnounce {
		mine := s.advertisedSnapshot()
		if len(mine) > 0 {
			if rp := s.encode(typeReply, mine); rp != nil {
				_, _ = s.conn.WriteTo(rp, remote)
			}
		}
	}
}

// encode builds a discovery packet (max 255 entries).
func (s *Service) encode(pType byte, adverts []Advert) []byte {
	if len(adverts) == 0 || len(adverts) > 255 {
		return nil
	}
	buf := make([]byte, 0, 6+len(adverts)*35)
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], magic)
	buf = append(buf, tmp[:]...)
	buf = append(buf, pType)
	buf = append(buf, byte(len(adverts)))
	for _, a := range adverts {
		buf = append(buf, a.InfoHash[:]...)
		var tp [2]byte
		binary.BigEndian.PutUint16(tp[:], uint16(s.tcpPort))
		buf = append(buf, tp[:]...)
		var flag byte
		if a.Complete {
			flag |= FlagComplete
		}
		buf = append(buf, flag)
	}
	return buf
}

// advertEntry is a decoded advertised file with its TCP port.
type advertEntry struct {
	InfoHash [32]byte
	TCPPort  int
	Complete bool
}

// tcpAddr builds the peer's TCP address from the UDP source IP + advertised port.
func (e *advertEntry) tcpAddr(remote net.Addr) string {
	host, _, err := net.SplitHostPort(remote.String())
	if err != nil {
		host = remote.String()
	}
	return net.JoinHostPort(host, itoa(e.TCPPort))
}

func decode(pkt []byte) (byte, []advertEntry, error) {
	if len(pkt) < 6 {
		return 0, nil, ErrBadPacket
	}
	if binary.BigEndian.Uint32(pkt[:4]) != magic {
		return 0, nil, ErrBadPacket
	}
	pType := pkt[4]
	count := int(pkt[5])
	entrySize := 32 + 2 + 1
	if len(pkt) < 6+count*entrySize {
		return 0, nil, ErrBadPacket
	}
	offset := 6
	entries := make([]advertEntry, 0, count)
	for i := 0; i < count; i++ {
		var ih [32]byte
		copy(ih[:], pkt[offset:offset+32])
		offset += 32
		port := int(binary.BigEndian.Uint16(pkt[offset : offset+2]))
		offset += 2
		flag := pkt[offset]
		offset++
		entries = append(entries, advertEntry{
			InfoHash: ih,
			TCPPort:  port,
			Complete: flag&FlagComplete != 0,
		})
	}
	return pType, entries, nil
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
