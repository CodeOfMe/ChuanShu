// Package store handles reading pieces from a source file and writing verified
// pieces into the download target. It creates the target file at full length
// up front (a sparse file on most filesystems) so pieces can be written at their
// correct offsets regardless of arrival order.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"

	"github.com/CodeOfMe/ChuanShu/internal/protocol"
)

// BuildMetadata reads a source file and produces transfer metadata: splits it
// into pieces and hashes each one. It streams the file so memory use stays flat
// even for very large files.
func BuildMetadata(path string, pieceLen int) (*protocol.Metadata, os.FileInfo, error) {
	if pieceLen <= 0 {
		pieceLen = protocol.DefaultPieceSize
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if st.IsDir() {
		return nil, nil, os.ErrInvalid
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	meta := &protocol.Metadata{
		Name:        filepath.Base(path),
		Length:      st.Size(),
		PieceLength: pieceLen,
	}
	buf := make([]byte, pieceLen)
	for {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			h := sha256.Sum256(buf[:n])
			digest := make([]byte, sha256.Size)
			copy(digest, h[:])
			meta.PieceHashes = append(meta.PieceHashes, digest)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return nil, nil, rerr
		}
	}
	if err := meta.Verify(); err != nil {
		return nil, nil, err
	}
	return meta, st, nil
}

// Store manages the local copy of a single file identified by its info hash.
type Store struct {
	Meta *protocol.Metadata
	// Target is the final path of the file (existing source or finished
	// download).
	Target string
	// partial is where an in-progress download lives until completion.
	partial string

	file *os.File
}

// OpenExisting opens a Store backed by an already-complete source file (for
// seeding). It does not create a partial file.
func OpenExisting(path string, meta *protocol.Metadata) (*Store, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &Store{Meta: meta, Target: path, file: f}, nil
}

// CreateDownload prepares a Store for an incoming download of meta's file. The
// finished file will be written to dir/meta.Name; a hidden .part file is used
// until all pieces are verified.
func CreateDownload(dir string, meta *protocol.Metadata) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	target := filepath.Join(dir, meta.Name)
	partial := target + ".part"
	// Open (or create) the partial without truncating so an interrupted download
	// can resume: already-written pieces survive and are re-verified by the
	// caller.
	f, err := os.OpenFile(partial, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	// Grow (never shrink) to the full length so pieces can be written at offset.
	if st, err := f.Stat(); err == nil && st.Size() < meta.Length {
		if err := f.Truncate(meta.Length); err != nil {
			f.Close()
			return nil, err
		}
	} else if meta.Length == 0 {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
	}
	return &Store{Meta: meta, Target: target, partial: partial, file: f}, nil
}

// Close releases the underlying file handle.
func (s *Store) Close() error {
	if s.file != nil {
		return s.file.Close()
	}
	return nil
}

// ReadPiece reads the piece data for a piece index from the current file.
func (s *Store) ReadPiece(index int) ([]byte, error) {
	size := s.Meta.PieceSize(index)
	if size == 0 {
		return nil, protocol.ErrBadFrame
	}
	buf := make([]byte, size)
	if _, err := s.file.ReadAt(buf, int64(index)*int64(s.Meta.PieceLength)); err != nil {
		return nil, err
	}
	return buf, nil
}

// HasPiece reports whether the piece at index matches its expected hash. This
// is how we discover an existing partial download's completed pieces on resume.
func (s *Store) HasPiece(index int) bool {
	data, err := s.ReadPiece(index)
	if err != nil {
		return false
	}
	h := sha256.Sum256(data)
	want := s.Meta.PieceHash(index)
	if len(want) != sha256.Size {
		return false
	}
	return hex.EncodeToString(h[:]) == hex.EncodeToString(want)
}

// WritePiece verifies data against the expected hash and, if it matches, writes
// it at the piece's offset. It returns ErrHashMismatch on a bad piece.
func (s *Store) WritePiece(index int, data []byte) error {
	size := s.Meta.PieceSize(index)
	if len(data) != size {
		return protocol.ErrHashMismatch
	}
	h := sha256.Sum256(data)
	want := s.Meta.PieceHash(index)
	if hex.EncodeToString(h[:]) != hex.EncodeToString(want) {
		return protocol.ErrHashMismatch
	}
	if s.file == nil {
		return os.ErrClosed
	}
	_, err := s.file.WriteAt(data, int64(index)*int64(s.Meta.PieceLength))
	return err
}

// DeletePartial closes the store and removes its partial file if present. It is
// a no-op for a store opened on an existing source file.
func (s *Store) DeletePartial() {
	if s.file != nil {
		s.file.Close()
		s.file = nil
	}
	if s.partial != "" {
		os.Remove(s.partial)
		s.partial = ""
	}
}

// Finalize flushes and renames the partial file to its final target name. It is
// a no-op for a Store that was opened on an existing source file.
func (s *Store) Finalize() error {
	if s.partial == "" {
		return nil
	}
	if err := s.file.Sync(); err != nil {
		return err
	}
	if err := s.file.Close(); err != nil {
		return err
	}
	if err := os.Rename(s.partial, s.Target); err != nil {
		return err
	}
	s.file = nil
	s.partial = ""
	return nil
}
