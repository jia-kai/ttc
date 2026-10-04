package tool

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"syscall"
	"ttc/internal/prompts"
	"unicode/utf8"

	"ttc/internal/blobcache"
	"ttc/internal/provider"
	"ttc/internal/workspace"
)

type readArgs struct {
	Path   string `json:"path"`
	Offset *int   `json:"offset,omitempty"`
	Limit  *int   `json:"limit,omitempty"`
}
type writeArgs struct {
	Path    string  `json:"path"`
	Content *string `json:"content"`
}
type editArgs struct {
	Path string  `json:"path"`
	Old  string  `json:"old_text"`
	New  *string `json:"new_text"`
	All  bool    `json:"replace_all,omitempty"`
}
type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Hidden  bool   `json:"hidden,omitempty"`
	Limit   *int   `json:"limit,omitempty"`
}
type grepArgs struct {
	Pattern       string `json:"pattern"`
	Path          string `json:"path,omitempty"`
	Include       string `json:"include,omitempty"`
	Literal       bool   `json:"literal,omitempty"`
	CaseSensitive *bool  `json:"case_sensitive,omitempty"`
	Limit         *int   `json:"limit,omitempty"`
}

func intDefault(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}
func rangeInt(name string, p *int, min, max int) error {
	if p != nil && (*p < min || *p > max) {
		return fmt.Errorf("%s must be %d–%d", name, min, max)
	}
	return nil
}
func validText(s string) error {
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
		return Fail("unsupported_content", "expected UTF-8 text without NUL")
	}
	return nil
}

// AddFiles registers read/search tools and serialized file mutations.
func AddFiles(r *Registry, w *workspace.Manager) {
	Register(r, "read", prompts.ToolDescription("read"), map[string]any{"path": Property("string"), "offset": Property("integer"), "limit": Property("integer")}, []string{"path"}, func(a readArgs) error {
		if e := Required("path", a.Path); e != nil {
			return e
		}
		if a.Offset != nil && *a.Offset < 1 {
			return errors.New("offset must be positive")
		}
		return rangeInt("limit", a.Limit, 1, 2000)
	}, func(ctx context.Context, x Execution, a readArgs) (any, error) {
		path := w.Path(a.Path)
		page, err := readPage(ctx, path, intDefault(a.Offset, 1), intDefault(a.Limit, 200), x.BinaryFiles)
		if err != nil {
			return nil, err
		}
		binary, ok := page.(binaryRead)
		if !ok {
			return page, nil
		}
		if a.Offset != nil || a.Limit != nil {
			return nil, Fail("invalid_input", "binary reads do not support offset or limit; omit pagination arguments")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hash := sha256.Sum256(binary.data)
		checksum := hex.EncodeToString(hash[:])
		cache, err := blobcache.Default()
		if err != nil {
			return nil, fmt.Errorf("open binary cache: %w", err)
		}
		if err := cache.Put(ctx, "original", checksum, binary.data); err != nil {
			return nil, fmt.Errorf("cache binary file: %w", err)
		}
		metadata := map[string]any{"kind": binary.kind, "path": path, "sha256": checksum, "mime_type": binary.mime, "bytes": len(binary.data), "truncated": false}
		if binary.kind == "image" {
			metadata["width"], metadata["height"] = binary.width, binary.height
		}
		return Output{
			Value: metadata,
			Files: []provider.BinaryFile{{Path: path, SHA256: checksum, MIMEType: binary.mime, Bytes: len(binary.data)}},
		}, nil
	})
	Register(r, "write", prompts.ToolDescription("write"), map[string]any{"path": Property("string"), "content": Property("string")}, []string{"path", "content"}, func(a writeArgs) error {
		if e := Required("path", a.Path); e != nil {
			return e
		}
		if a.Content == nil {
			return errors.New("content required")
		}
		return validText(*a.Content)
	}, func(ctx context.Context, x Execution, a writeArgs) (any, error) {
		res, e := w.Apply(ctx, x.SessionID, x.CallID, []workspace.Mutation{{Path: a.Path, Data: []byte(*a.Content)}})
		value := map[string]any{"path": w.Path(a.Path), "bytes": len(*a.Content)}
		if len(res.Changes) != 0 {
			value["created"] = !res.Changes[0].Before.Exists
		}
		return presentFiles(ctx, value, res.Changes), e
	})
	Register(r, "edit", prompts.ToolDescription("edit"), map[string]any{"path": Property("string"), "old_text": Property("string"), "new_text": Property("string"), "replace_all": Property("boolean")}, []string{"path", "old_text", "new_text"}, func(a editArgs) error {
		if e := Required("path", a.Path); e != nil {
			return e
		}
		if a.Old == "" || a.New == nil || a.Old == *a.New {
			return errors.New("old_text must be nonempty and new_text must be present and different; new_text may be empty to delete text")
		}
		if e := validText(a.Old); e != nil {
			return e
		}
		return validText(*a.New)
	}, func(ctx context.Context, x Execution, a editArgs) (any, error) {
		count := 0
		res, e := w.Apply(ctx, x.SessionID, x.CallID, []workspace.Mutation{{Path: a.Path, MustExist: true, Transform: func(b []byte) ([]byte, error) {
			if e := validText(string(b)); e != nil {
				return nil, e
			}
			count = strings.Count(string(b), a.Old)
			if count == 0 {
				return nil, Fail("ambiguous_match", "old_text matched 0 times; read current contents and adjust old_text to match exactly")
			}
			if !a.All && count != 1 {
				return nil, Fail("ambiguous_match", fmt.Sprintf("old_text matched %d times; include more surrounding context or set replace_all=true to replace every match", count))
			}
			// Matched old text cannot exceed the original file. Divide the
			// remaining allowance so repeated replacements cannot overflow an
			// integer or allocate an oversized result before Apply rejects it.
			remaining := len(b) - count*len(a.Old)
			if remaining > workspace.MaxFileBytes || len(*a.New) > (workspace.MaxFileBytes-remaining)/count {
				return nil, Fail("file_too_large", "replacement exceeds the 8 MiB file limit; reduce new_text or replace fewer occurrences")
			}
			return []byte(strings.ReplaceAll(string(b), a.Old, *a.New)), nil
		}}})
		return presentFiles(ctx, map[string]any{"path": w.Path(a.Path), "replacements": count}, res.Changes), e
	})
	addSearch(r, w)
	addPatch(r, w)
}
func readPage(ctx context.Context, path string, offset, limit int, types []provider.BinaryFileType) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Decide the file kind from the opened descriptor: a build or shell can
	// replace the path between stat and open, including with a blocking FIFO.
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()
	page, err := readOpenedPage(ctx, f, path, offset, limit, types)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return page, err
}

// readOpenedPage consumes the caller-owned descriptor, never reopening path.
func readOpenedPage(ctx context.Context, f *os.File, path string, offset, limit int, types []provider.BinaryFileType) (any, error) {
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if st.IsDir() {
		entries, e := readDirectoryEntries(ctx, f)
		if e != nil {
			return nil, e
		}
		out := []map[string]string{}
		start := offset - 1
		if start > len(entries) {
			start = len(entries)
		}
		end := start + limit
		if end > len(entries) {
			end = len(entries)
		}
		for _, d := range entries[start:end] {
			kind := "other"
			if d.Type()&fs.ModeSymlink != 0 {
				kind = "symlink"
			} else if d.IsDir() {
				kind = "directory"
			} else if d.Type().IsRegular() {
				kind = "file"
			}
			out = append(out, map[string]string{"name": d.Name(), "type": kind})
		}
		var next any
		if end < len(entries) {
			next = end + 1
		}
		return map[string]any{"kind": "directory", "path": path, "entries": out, "next_offset": next, "truncated": next != nil}, nil
	}
	if !st.Mode().IsRegular() {
		return nil, Fail("unsupported_content", "not a regular file")
	}
	reader := bufio.NewReader(f)
	// Sniff only the opened descriptor, not the extension or a reopened path.
	// Peek does not consume text bytes or validate content outside its page.
	header, err := reader.Peek(8)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if kind, mime, extension := binaryKind(path, header, types); kind != "" {
		return readBinary(ctx, reader, st.Size(), kind, mime, extension, types)
	}
	var content strings.Builder
	line := 1
	count := 0
	var next any
	for {
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		if count >= limit {
			if _, e := reader.Peek(1); e == io.EOF {
				break
			} else if e != nil {
				return nil, e
			}
			next = line
			break
		}
		part, e := boundedLine(reader)
		if e != nil && e != io.EOF {
			var failure *Error
			if count > 0 && errors.As(e, &failure) && failure.Code == "line_too_long" {
				next = line
				break
			}
			return nil, e
		}
		if e != nil && len(part) == 0 {
			break
		}
		if line >= offset {
			if content.Len()+len(part) > 40000 {
				next = line
				break
			}
		}
		if err := validText(part); err != nil {
			return nil, Fail("unsupported_content", "expected UTF-8 text without NUL; no supported binary format was recognized; provide an announced original format or use shell for explicit inspection/conversion")
		}
		if line >= offset {
			content.WriteString(part)
			count++
		}
		line++
		if e != nil {
			break
		}
	}
	return map[string]any{"kind": "file", "path": path, "content": content.String(), "start_line": offset, "next_offset": next, "truncated": next != nil}, nil
}

type binaryRead struct {
	data          []byte
	mime          string
	kind          string
	width, height int
}

func imageMIME(header []byte) string {
	switch {
	case bytes.HasPrefix(header, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(header, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(header, []byte("GIF87a")), bytes.HasPrefix(header, []byte("GIF89a")):
		return "image/gif"
	default:
		return ""
	}
}

const directoryEntryLimit = 10000

// readDirectoryEntries preserves sorted pagination without unbounded directory
// allocation. Larger directories should be narrowed with ripgrep-backed glob.
func readDirectoryEntries(ctx context.Context, f *os.File) ([]fs.DirEntry, error) {
	var entries []fs.DirEntry
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := f.ReadDir(256)
		if len(entries)+len(batch) > directoryEntryLimit {
			return nil, Fail("directory_too_large", "directory exceeds 10000 entries; use glob with a narrower pattern or read a subdirectory")
		}
		entries = append(entries, batch...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, ctx.Err()
}

// DecodeResult is a convenience for callers that inspect common result envelopes.
func DecodeResult(b []byte) (map[string]json.RawMessage, error) {
	var r map[string]json.RawMessage
	e := json.Unmarshal(b, &r)
	return r, e
}

func boundedLine(reader *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		chunk, e := reader.ReadSlice('\n')
		if b.Len()+len(chunk) > 40000 {
			return "", Fail("line_too_long", "line exceeds 40000-byte page cap; use shell to extract a bounded byte range instead")
		}
		b.Write(chunk)
		if e == bufio.ErrBufferFull {
			continue
		}
		return b.String(), e
	}
}
