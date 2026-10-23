package history

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"ttc/internal/messagestream"
)

// rawManifest indexes UTF-8-aligned content-defined chunks by byte and rune
// count. Paging skips unneeded chunks using metadata and verifies only chunks
// actually read. Hash is omitted for composed readable views, whose individually
// verified chunks still retain exact text.
type rawManifest struct {
	Bytes  int             `json:"bytes"`
	Chars  int             `json:"chars"`
	Hash   string          `json:"sha256,omitempty"`
	Chunks []boundaryChunk `json:"chunks"`
}
type boundaryChunk struct {
	Hash  string `json:"hash"`
	Bytes int    `json:"bytes"`
	Chars int    `json:"chars"`
}

const boundaryChunkBytes = (128 << 10) + 4

func (s *Store) boundaryArtifact(session string, data []byte) (string, error) {
	v, err := s.Session(session)
	if err != nil {
		return "", err
	}
	return s.boundaryArtifactInLineage(v.LineageID, data)
}

func (s *Store) boundaryArtifactInLineage(lineage string, data []byte) (string, error) {
	if int64(len(data)) > boundaryArtifactBytes {
		return "", errors.New("boundary artifact exceeds 256 MiB")
	}
	m := rawManifest{Bytes: len(data), Chars: utf8.RuneCount(data), Hash: messagestream.Hash(data)}
	const minChunk = 16 << 10
	const maxChunk = 128 << 10
	const window = 64
	var rolling uint64
	var ring [window]byte
	start := 0
	const base uint64 = 257
	power := uint64(1)
	for i := 0; i < window; i++ {
		power *= base
	}
	for i, c := range data {
		old := ring[i%window]
		ring[i%window] = c
		rolling = rolling*base + uint64(c) - uint64(old)*power
		size := i + 1 - start
		end := i + 1
		cut := size >= maxChunk || (size >= minChunk && (rolling&0xffff) == 0) || end == len(data)
		if cut && (end == len(data) || utf8.RuneStart(data[end])) {
			chunk := data[start:end]
			p, err := s.artifactInLineage(lineage, "boundarychunks", chunk)
			if err != nil {
				return "", err
			}
			m.Chunks = append(m.Chunks, boundaryChunk{Hash: filepath.Base(p), Bytes: len(chunk), Chars: utf8.RuneCount(chunk)})
			start = end
		}
	}
	return s.saveBoundaryManifestInLineage(lineage, m)
}
func (s *Store) saveBoundaryManifestInLineage(lineage string, m rawManifest) (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	if len(b) > 16<<20 {
		return "", errors.New("boundary manifest exceeds 16 MiB")
	}
	return s.artifactInLineage(lineage, "boundaryraw", b)
}

func openBoundaryRaw(ctx context.Context, path string) (*boundaryChunkReader, error) {
	b, err := readBoundaryCAS(path, 16<<20)
	if err != nil {
		return nil, err
	}
	var m rawManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Bytes < 0 || int64(m.Bytes) > boundaryArtifactBytes || m.Chars < 0 || m.Chars > m.Bytes || len(m.Chunks) > 1000000 {
		return nil, errors.New("invalid boundary manifest")
	}
	bytes, chars := 0, 0
	for _, chunk := range m.Chunks {
		if _, err := hex.DecodeString(chunk.Hash); err != nil || len(chunk.Hash) != 64 || chunk.Bytes < 1 || chunk.Bytes > boundaryChunkBytes || chunk.Chars < 1 || chunk.Chars > chunk.Bytes {
			return nil, errors.New("invalid boundary chunk index")
		}
		bytes += chunk.Bytes
		chars += chunk.Chars
	}
	if bytes != m.Bytes || chars != m.Chars {
		return nil, errors.New("boundary manifest totals mismatch")
	}
	return &boundaryChunkReader{ctx: ctx, manifest: m, root: filepath.Join(filepath.Dir(filepath.Dir(path)), "boundarychunks")}, nil
}

func readBoundaryCAS(path string, limit int64) ([]byte, error) {
	data, err := readArtifact(path, limit)
	if err != nil {
		return nil, err
	}
	if messagestream.Hash(data) != filepath.Base(path) {
		return nil, errors.New("boundary manifest hash mismatch")
	}
	return data, nil
}

type boundaryChunkReader struct {
	ctx               context.Context
	manifest          rawManifest
	index, pos, total int
	data              []byte
	root              string
}

func (r *boundaryChunkReader) chunk(index int) ([]byte, error) {
	return readBoundaryChunk(r.ctx, r.root, r.manifest.Chunks[index])
}

func readBoundaryChunk(ctx context.Context, root string, c boundaryChunk) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, err := readArtifact(filepath.Join(root, c.Hash), boundaryChunkBytes)
	if err != nil {
		return nil, err
	}
	if len(b) != c.Bytes || utf8.RuneCount(b) != c.Chars || messagestream.Hash(b) != c.Hash {
		return nil, errors.New("boundary chunk integrity mismatch")
	}
	return b, nil
}
func (r *boundaryChunkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	for r.pos == len(r.data) {
		if r.index == len(r.manifest.Chunks) {
			if r.total != r.manifest.Bytes {
				return 0, errors.New("boundary raw byte count mismatch")
			}
			return 0, io.EOF
		}
		b, err := r.chunk(r.index)
		if err != nil {
			return 0, err
		}
		r.index++
		r.pos = 0
		r.data = b
		r.total += len(b)
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func boundaryTextPage(ctx context.Context, path string, offset, limit int) (string, int, error) {
	r, err := openBoundaryRaw(ctx, path)
	if err != nil {
		return "", 0, err
	}
	var out strings.Builder
	remaining := limit
	for index, c := range r.manifest.Chunks {
		if remaining == 0 {
			break
		}
		if offset >= c.Chars {
			offset -= c.Chars
			continue
		}
		data, err := r.chunk(index)
		if err != nil {
			return "", 0, err
		}
		for _, ch := range string(data) {
			if offset > 0 {
				offset--
				continue
			}
			if remaining == 0 {
				break
			}
			out.WriteRune(ch)
			remaining--
		}
	}
	return out.String(), r.manifest.Chars, nil
}
func readBoundaryAll(ctx context.Context, path string) ([]byte, error) {
	r, err := openBoundaryRaw(ctx, path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if r.manifest.Hash != "" && messagestream.Hash(data) != r.manifest.Hash {
		return nil, errors.New("boundary raw hash mismatch")
	}
	return data, nil
}
