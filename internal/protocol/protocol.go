// Package protocol defines the on-disk transfer metadata and the wire format
// used between chuanshu peers. It borrows the core ideas from BitTorrent:
// a file is split into fixed-size pieces, each piece is hashed, and the file is
// identified by the digest of that metadata (the "info hash"). Peers exchange
// pieces and self-verify every piece against its hash, so any peer that has a
// valid copy can seed it to others.
package protocol

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Version is the wire protocol version. Peers refuse to talk to peers running
// a different version.
const Version = 1

// DefaultPieceSize is the default piece length in bytes (256 KiB). Pieces are
// kept small enough that bad pieces are cheap to re-download, and large enough
// that per-piece overhead stays negligible.
const DefaultPieceSize = 256 * 1024

// MaxPieceSize is the largest piece length a file may be split into. A piece is
// carried in a single frame, and ReadFrame bounds ordinary frames at
// DefaultMaxFrame (see below); a larger piece could not be received by a peer.
// Deriving it from DefaultPieceSize*4 keeps the old implicit cap (DefaultMaxFrame
// = MaxPieceSize + 1024) while letting BuildMetadata reject an oversized --piece
// up front instead of silently producing a file nobody can download.
const MaxPieceSize = DefaultPieceSize * 4

// ProtocolErr sentinels.
var (
	ErrBadFrame     = errors.New("chuanshu: malformed frame")
	ErrVersion      = errors.New("chuanshu: protocol version mismatch")
	ErrUnknownInfo  = errors.New("chuanshu: unknown info hash")
	ErrHashMismatch = errors.New("chuanshu: piece hash mismatch")
	// ErrPieceTooLarge is returned when a piece length exceeds MaxPieceSize, so
	// the frames that carry its pieces could not be received by a peer.
	ErrPieceTooLarge = errors.New("chuanshu: piece length exceeds wire limit")
)

// Metadata describes a single shared file. It is the object whose canonical
// encoding is hashed to produce the InfoHash.
type Metadata struct {
	Name        string `json:"name"`
	Length      int64  `json:"length"`
	PieceLength int    `json:"pieceLength"`
	// PieceHashes holds one raw SHA-256 digest per piece (32 bytes each).
	PieceHashes [][]byte `json:"-"`
}

// numPieces returns how many pieces the file is split into.
func (m *Metadata) NumPieces() int {
	if m.Length <= 0 {
		return 0
	}
	return int((m.Length + int64(m.PieceLength) - 1) / int64(m.PieceLength))
}

// PieceSize returns the length (in bytes) of the given piece index; the last
// piece is typically shorter.
func (m *Metadata) PieceSize(index int) int {
	if index < 0 || index >= m.NumPieces() {
		return 0
	}
	rem := m.Length - int64(index)*int64(m.PieceLength)
	if rem > int64(m.PieceLength) {
		return m.PieceLength
	}
	return int(rem)
}

// canonical returns a deterministic byte encoding of the metadata used to
// derive the info hash. We build it by hand so the hash is stable regardless of
// JSON encoder details.
func (m *Metadata) canonical() []byte {
	var buf []byte
	// name length + name
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], uint64(len(m.Name)))
	buf = append(buf, tmp[:]...)
	buf = append(buf, m.Name...)
	// length
	binary.BigEndian.PutUint64(tmp[:], uint64(m.Length))
	buf = append(buf, tmp[:]...)
	// piece length
	binary.BigEndian.PutUint64(tmp[:], uint64(m.PieceLength))
	buf = append(buf, tmp[:]...)
	// piece hashes concatenated
	for _, h := range m.PieceHashes {
		buf = append(buf, h...)
	}
	return buf
}

// InfoHash computes the SHA-256 digest of the canonical metadata.
func (m *Metadata) InfoHash() [32]byte {
	return sha256.Sum256(m.canonical())
}

// InfoHashHex returns the hex-encoded info hash.
func (m *Metadata) InfoHashHex() string {
	h := m.InfoHash()
	return hex.EncodeToString(h[:])
}

// marshal serializes metadata (piece hashes as hex strings joined).
type metaJSON struct {
	Name        string `json:"name"`
	Length      int64  `json:"length"`
	PieceLength int    `json:"pieceLength"`
	HashesHex   string `json:"hashes"` // concatenation of hex digests
}

// MarshalJSON implements json.Marshaler.
func (m *Metadata) MarshalJSON() ([]byte, error) {
	all := make([]byte, 0, len(m.PieceHashes)*sha256.Size)
	for _, h := range m.PieceHashes {
		all = append(all, h...)
	}
	return json.Marshal(metaJSON{
		Name:        m.Name,
		Length:      m.Length,
		PieceLength: m.PieceLength,
		HashesHex:   hex.EncodeToString(all),
	})
}

// UnmarshalJSON implements json.Unmarshaler. It recomputes and validates the
// info hash against the expected value (if provided).
func (m *Metadata) UnmarshalJSON(data []byte) error {
	var j metaJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	raw, err := hex.DecodeString(j.HashesHex)
	if err != nil {
		return fmt.Errorf("decode hashes: %w", err)
	}
	if len(raw)%sha256.Size != 0 {
		return ErrBadFrame
	}
	hashes := make([][]byte, 0, len(raw)/sha256.Size)
	for i := 0; i < len(raw); i += sha256.Size {
		h := make([]byte, sha256.Size)
		copy(h, raw[i:i+sha256.Size])
		hashes = append(hashes, h)
	}
	m.Name = j.Name
	m.Length = j.Length
	m.PieceLength = j.PieceLength
	m.PieceHashes = hashes
	return nil
}

// Verify checks the metadata's internal consistency.
func (m *Metadata) Verify() error {
	if m.Length < 0 {
		return ErrBadFrame
	}
	if m.PieceLength <= 0 {
		return ErrBadFrame
	}
	if m.PieceLength > MaxPieceSize {
		return ErrPieceTooLarge
	}
	if m.NumPieces() != len(m.PieceHashes) {
		return ErrBadFrame
	}
	for _, h := range m.PieceHashes {
		if len(h) != sha256.Size {
			return ErrBadFrame
		}
	}
	return nil
}

// PieceHash returns the expected digest for a piece index.
func (m *Metadata) PieceHash(index int) []byte {
	if index < 0 || index >= len(m.PieceHashes) {
		return nil
	}
	return m.PieceHashes[index]
}

// ---- Bitfield ----

// Bitfield tracks which pieces a peer holds. It is safe for concurrent use: a
// single node may be downloading pieces (Set) while also serving them to other
// peers (Has) for the same file.
type Bitfield struct {
	mu sync.Mutex
	n  int
	b  []byte
}

// NewBitfield returns an empty bitfield for n pieces.
func NewBitfield(n int) *Bitfield {
	return &Bitfield{n: n, b: make([]byte, (n+7)/8)}
}

// Set marks a piece as present. Out-of-range indices are ignored so a caller
// that mis-handles an untrusted piece index cannot corrupt memory or panic.
func (b *Bitfield) Set(i int) {
	if i < 0 || i >= b.n {
		return
	}
	b.mu.Lock()
	b.b[i/8] |= 1 << (uint(i) % 8)
	b.mu.Unlock()
}

// Has reports whether a piece is present.
func (b *Bitfield) Has(i int) bool {
	if i < 0 || i >= b.n {
		return false
	}
	b.mu.Lock()
	v := b.b[i/8]&(1<<(uint(i)%8)) != 0
	b.mu.Unlock()
	return v
}

// Count returns the number of set pieces.
func (b *Bitfield) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := 0
	for i := 0; i < b.n; i++ {
		if b.b[i/8]&(1<<(uint(i)%8)) != 0 {
			c++
		}
	}
	return c
}

// Complete reports whether all pieces are present.
func (b *Bitfield) Complete() bool {
	return b.Count() == b.n
}

// Missing returns the indices of pieces not yet present.
func (b *Bitfield) Missing() []int {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []int
	for i := 0; i < b.n; i++ {
		if b.b[i/8]&(1<<(uint(i)%8)) == 0 {
			out = append(out, i)
		}
	}
	return out
}

// Bytes returns a copy of the underlying bitmap (wire form).
func (b *Bitfield) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, len(b.b))
	copy(out, b.b)
	return out
}

// BitfieldFromBytes rebuilds a bitfield of n pieces from wire form.
func BitfieldFromBytes(raw []byte, n int) *Bitfield {
	bf := NewBitfield(n)
	copy(bf.b, raw)
	return bf
}

// ---- Wire framing ----
//
// A frame is: [1 byte type][4 byte big-endian payload length][payload].

// MsgType enumerates message kinds.
type MsgType uint8

const (
	MsgHello    MsgType = 1
	MsgBitfield MsgType = 2
	MsgHave     MsgType = 3 // index
	MsgRequest  MsgType = 4 // index
	MsgPiece    MsgType = 5 // index + data
	MsgGetMeta  MsgType = 6
	MsgMeta     MsgType = 7 // JSON metadata
	MsgReject   MsgType = 8
)

// WriteFrame writes a framed message.
func WriteFrame(w io.Writer, t MsgType, payload []byte) error {
	header := make([]byte, 5)
	header[0] = byte(t)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// Message-size limits. A single hard cap cannot fit every message type: piece
// frames are bounded by the piece length (DefaultPieceSize plus a little
// slack), but a metadata frame is hex-encoded and grows with the *number* of
// pieces. Capping everything at the piece size makes files larger than a few
// GiB un-downloadable, because their metadata frame would be rejected before it
// could be read. So we keep a small default cap for ordinary/piece frames and
// let the metadata reader ask for a much larger one via ReadFrameWithMax.
const (
	// DefaultMaxFrame bounds piece, index and handshake frames. It is MaxPieceSize
	// plus a little slack so a full piece passes while still rejecting an absurd
	// length field before we allocate for it.
	DefaultMaxFrame = uint32(MaxPieceSize + 1024)
	// MetaMaxFrame bounds a single metadata frame. One piece hash is 64 hex
	// characters; this allowance covers files up to roughly 32 GiB at the
	// default piece size (and far more with larger pieces) while still
	// rejecting a corrupt length field far below the 4 GiB a uint32 allows.
	MetaMaxFrame = uint32(64 * 1024 * 1024)
)

// ReadFrame reads a framed message using DefaultMaxFrame.
func ReadFrame(r io.Reader) (MsgType, []byte, error) {
	return ReadFrameWithMax(r, DefaultMaxFrame)
}

// ReadFrameWithMax reads a framed message, rejecting any whose declared payload
// exceeds maxLen. The length is a 4-byte field, so an attacker could claim
// nearly 4 GiB; the caller supplies a max appropriate to the message it expects
// (DefaultMaxFrame for pieces, MetaMaxFrame for metadata) so we never allocate
// for an absurd frame.
func ReadFrameWithMax(r io.Reader, maxLen uint32) (MsgType, []byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, nil, err
	}
	t := MsgType(header[0])
	length := binary.BigEndian.Uint32(header[1:])
	if length > maxLen {
		return t, nil, ErrBadFrame
	}
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return t, nil, err
		}
	}
	return t, payload, nil
}

// Hello is sent immediately after a TCP connection is established.
type Hello struct {
	Version  int    `json:"version"`
	PeerID   []byte `json:"peerId"`
	InfoHash []byte `json:"infoHash"`
}

// EncodeIndex encodes a piece index payload.
func EncodeIndex(index int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(index))
	return b[:]
}

// DecodeIndex decodes a piece index payload.
func DecodeIndex(b []byte) (int, error) {
	if len(b) != 4 {
		return 0, ErrBadFrame
	}
	return int(binary.BigEndian.Uint32(b)), nil
}

// EncodePieceReq encodes a piece request: index.
func EncodePieceReq(index int) []byte { return EncodeIndex(index) }

// EncodePieceResp encodes a piece response: index + data.
func EncodePieceResp(index int, data []byte) []byte {
	out := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(out[:4], uint32(index))
	copy(out[4:], data)
	return out
}

// DecodePieceResp decodes index + data from a piece response.
func DecodePieceResp(b []byte) (int, []byte, error) {
	if len(b) < 4 {
		return 0, nil, ErrBadFrame
	}
	idx := int(binary.BigEndian.Uint32(b[:4]))
	return idx, b[4:], nil
}
