package main

import (
	"os"

	"github.com/CodeOfMe/ChuanShu/internal/protocol"
	"github.com/CodeOfMe/ChuanShu/internal/store"
)

// buildMeta computes transfer metadata for a file without starting a node. It
// backs the "hash" command so users can obtain a file's info hash up front.
func buildMeta(path string, pieceLen int) (*protocol.Metadata, os.FileInfo, error) {
	return store.BuildMetadata(path, pieceLen)
}
