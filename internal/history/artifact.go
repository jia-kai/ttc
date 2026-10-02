package history

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"syscall"
)

const instructionSnapshotBytes = 1 << 20

// readArtifact rejects special files and final symlinks before reading. The
// caller supplies a byte bound, checked against both metadata and actual bytes
// so concurrent growth cannot turn a small immutable snapshot into a large read.
func readArtifact(path string, maxBytes int64) ([]byte, error) {
	f, err := openArtifact(path, maxBytes)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("saved artifact exceeds %d-byte limit", maxBytes)
	}
	return data, nil
}

// openArtifact shares the file-type and metadata checks with paged instruction
// readers, which enforce the same byte bound while streaming instead of loading
// the complete snapshot.
func openArtifact(path string, maxBytes int64) (*os.File, error) {
	if maxBytes < 0 || maxBytes == math.MaxInt64 {
		return nil, errors.New("invalid saved artifact byte limit")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("saved artifact must be a regular file")
	}
	if info.Size() > maxBytes {
		f.Close()
		return nil, fmt.Errorf("saved artifact exceeds %d-byte limit", maxBytes)
	}
	return f, nil
}
