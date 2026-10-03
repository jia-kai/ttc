// Package workspace serializes file-tool edits and restoration within one runtime.
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
	"scicode/internal/history"
)

// MaxFileBytes bounds each mutation's original and resulting file contents.
// Transform callers must check expansion before allocating their result.
const MaxFileBytes = 8 << 20

// State represents absence or immutable bytes and permission bits.
type State struct {
	Exists bool   `json:"exists"`
	Mode   uint32 `json:"mode,omitempty"`
	Hash   string `json:"hash,omitempty"`
	Blob   string `json:"blob,omitempty"`
}

// PathChange records exact before/after state and actual application outcome.
type PathChange struct {
	Path        string   `json:"path"`
	Before      State    `json:"before"`
	After       State    `json:"after"`
	Applied     bool     `json:"applied"`
	CreatedDirs []string `json:"created_dirs,omitempty"`
}

// Mutation requests complete bytes or deletion. An optional Transform runs under the queue lock.
type Mutation struct {
	Source     string // Optional source file read under the mutation lock (for moves).
	Path       string
	Delete     bool
	Data       []byte
	Transform  func([]byte) ([]byte, error)
	MustExist  bool
	MustAbsent bool
}

// Result describes the applied subset; partial failures retain their change ID.
type Result struct {
	ChangeID   int64
	Reversible bool
	Changes    []PathChange
}

// Manager owns the local mutation queue shared by one runtime's actors.
type Manager struct {
	Root  string
	Store *history.Store
	mu    sync.Mutex
}

// Open resolves the workspace without locking it or recovering filesystem changes.
func Open(root string, store *history.Store) (*Manager, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("workspace must be a directory")
	}
	return &Manager{Root: root, Store: store}, nil
}

// Path resolves a path without granting symlink mutation access.
func (m *Manager) Path(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(m.Root, path)
}
func safePath(path string) error {
	path = filepath.Clean(path)
	for p := path; ; p = filepath.Dir(p) {
		st, e := os.Lstat(p)
		if e != nil {
			if os.IsNotExist(e) {
				if p == filepath.Dir(p) {
					break
				}
				continue
			}
			return e
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink mutation path: %s; resolve the real target and retry with its path", p)
		}
		if p == path {
			if !st.Mode().IsRegular() {
				return fmt.Errorf("not a regular file: %s", p)
			}
			sys, ok := st.Sys().(*syscall.Stat_t)
			if !ok || sys.Nlink > 1 {
				return fmt.Errorf("multiply linked file: %s", p)
			}
		} else if !st.IsDir() {
			return fmt.Errorf("parent is not a directory: %s", p)
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	return nil
}
func hash(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func readFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", path)
	}
	if st.Size() > MaxFileBytes {
		return nil, errors.New("file exceeds 8 MiB mutation limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, errors.New("file exceeds 8 MiB mutation limit")
	}
	return data, nil
}
func (m *Manager) capture(session, path string) (State, []byte, error) {
	if e := safePath(path); e != nil {
		return State{}, nil, e
	}
	st, e := os.Lstat(path)
	if os.IsNotExist(e) {
		return State{}, nil, nil
	}
	if e != nil {
		return State{}, nil, e
	}
	if st.Size() > MaxFileBytes {
		return State{}, nil, errors.New("file exceeds 8 MiB mutation limit")
	}
	data, e := readFile(path)
	if e != nil {
		return State{}, nil, e
	}
	blob, e := m.Store.Artifact(session, "snapshots", data)
	return State{true, uint32(st.Mode().Perm()), hash(data), blob}, data, e
}
func current(path string, s State) error {
	if e := safePath(path); e != nil {
		return e
	}
	st, e := os.Lstat(path)
	if !s.Exists && os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if !s.Exists || uint32(st.Mode().Perm()) != s.Mode {
		return fmt.Errorf("workspace conflict: %s", path)
	}
	data, e := readFile(path)
	if e != nil {
		return e
	}
	if hash(data) != s.Hash {
		return fmt.Errorf("workspace conflict: %s", path)
	}
	return nil
}
func missingDirs(path string) []string {
	var out []string
	for p := filepath.Dir(path); ; p = filepath.Dir(p) {
		_, e := os.Lstat(p)
		if e == nil {
			break
		}
		if !os.IsNotExist(e) {
			break
		}
		out = append(out, p)
		if p == filepath.Dir(p) {
			break
		}
	}
	return out
}
func apply(path string, s State) error {
	if e := safePath(path); e != nil {
		return e
	}
	if !s.Exists {
		if e := os.Remove(path); e != nil && !os.IsNotExist(e) {
			return e
		}
		return history.SyncDir(filepath.Dir(path))
	}
	data, e := readFile(s.Blob)
	if e != nil {
		return e
	}
	if hash(data) != s.Hash {
		return errors.New("corrupt snapshot blob")
	}
	if e = os.MkdirAll(filepath.Dir(path), 0755); e != nil {
		return e
	}
	return history.AtomicFile(path, data, os.FileMode(s.Mode))
}

// Apply serializes read, validation, filesystem writes and history commit locally.
// Filesystem effects and the history transaction are independent; neither is recovered.
func (m *Manager) Apply(ctx context.Context, session, call string, ops []Mutation) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	metadata, err := m.Store.Session(session)
	if err != nil {
		return Result{}, err
	}
	if metadata.CompactionError != "" {
		return Result{}, fmt.Errorf("session unusable after compaction: %s", metadata.CompactionError)
	}
	if len(ops) == 0 {
		return Result{}, errors.New("empty mutation")
	}
	if e := ctx.Err(); e != nil {
		return Result{}, e
	}
	reversible := true
	var changes []PathChange
	seen := map[string]bool{}
	for _, op := range ops {
		path := m.Path(op.Path)
		if seen[path] {
			return Result{}, fmt.Errorf("duplicate mutation target %s", path)
		}
		seen[path] = true
		before, data, e := m.capture(session, path)
		if e != nil {
			return Result{}, e
		}
		if op.MustExist && !before.Exists {
			return Result{}, fmt.Errorf("source not found: %s", path)
		}
		if op.MustAbsent && before.Exists {
			return Result{}, fmt.Errorf("target exists: %s; read it and use an update for intended changes, or choose a different add/move destination", path)
		}
		after := State{}
		sourceMode := uint32(0)
		if !op.Delete {
			if op.Source != "" {
				var source State
				source, data, e = m.capture(session, m.Path(op.Source))
				sourceMode = source.Mode
				if e != nil {
					return Result{}, e
				}
			}
			if op.Transform != nil {
				data, e = op.Transform(data)
				if e != nil {
					return Result{}, e
				}
			} else {
				data = op.Data
			}
			if len(data) > MaxFileBytes {
				return Result{}, errors.New("file exceeds 8 MiB mutation limit")
			}
			blob, e := m.Store.Artifact(session, "snapshots", data)
			if e != nil {
				return Result{}, e
			}
			mode := uint32(0644)
			if sourceMode != 0 {
				mode = sourceMode
			}
			if before.Exists {
				mode = before.Mode
			}
			after = State{true, mode, hash(data), blob}
		}
		rel, e := filepath.Rel(m.Root, path)
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			reversible = false
		}
		changes = append(changes, PathChange{Path: path, Before: before, After: after, CreatedDirs: missingDirs(path)})
	}
	var failure error
	for i := range changes {
		c := &changes[i]
		if e := ctx.Err(); e != nil {
			failure = e
			break
		}
		if e := current(c.Path, c.Before); e != nil {
			failure = e
			break
		}
		if e := apply(c.Path, c.After); e != nil {
			failure = e // A rename may have succeeded before fsync failed.
			if current(c.Path, c.After) == nil {
				c.Applied = true
			}
			break
		}
		c.Applied = true
	}
	applied := 0
	for _, c := range changes {
		if c.Applied {
			applied++
		}
	}
	for _, c := range changes {
		if !c.Applied {
			c.After = State{}
			cleanupDirs([]PathChange{c})
		}
	}
	if applied == 0 {
		return Result{}, failure
	}
	id, e := m.Store.CommitChange(session, call, changes, reversible)
	r := Result{id, reversible, changes}
	if e != nil {
		return r, fmt.Errorf("record file mutation: %w", e)
	}
	if failure != nil {
		return r, fmt.Errorf("partial mutation: %w", failure)
	}
	return r, nil
}

// Restore checks affected paths and applies saved bytes without executing tools.
// A partial failure leaves the cursor unchanged; no automatic repair is attempted.
func (m *Manager) Restore(ctx context.Context, session string, target history.RestoreTarget) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, e := m.Store.Session(session)
	if e != nil {
		return e
	}
	old, e := m.Store.Changes(v.FileTip)
	if e != nil {
		return e
	}
	newc, e := m.Store.Changes(target.FileTip)
	if e != nil {
		return e
	}
	anc := map[int64]bool{0: true}
	for _, c := range newc {
		anc[c.ID] = true
	}
	common := int64(0)
	for _, c := range old {
		if anc[c.ID] {
			common = c.ID
			break
		}
	}
	changes := map[string]*PathChange{}
	add := func(c history.Change, reverse bool) error {
		if !c.Reversible {
			return errors.New("restoration crosses a non-undoable outside-workspace edit")
		}
		var paths []PathChange
		if e := json.Unmarshal(c.Paths, &paths); e != nil {
			return e
		}
		if reverse {
			for i, j := 0, len(paths)-1; i < j; i, j = i+1, j-1 {
				paths[i], paths[j] = paths[j], paths[i]
			}
		}
		for _, p := range paths {
			if !p.Applied {
				continue
			}
			from, to := p.Before, p.After
			if reverse {
				from, to = to, from
			}
			if existing := changes[p.Path]; existing != nil {
				if existing.After.Exists != from.Exists || existing.After.Hash != from.Hash || existing.After.Mode != from.Mode {
					return errors.New("inconsistent file change chain")
				}
				existing.After = to
				existing.CreatedDirs = append(existing.CreatedDirs, p.CreatedDirs...)
			} else {
				changes[p.Path] = &PathChange{Path: p.Path, Before: from, After: to, CreatedDirs: p.CreatedDirs}
			}
		}
		return nil
	}
	for _, c := range old {
		if c.ID == common {
			break
		}
		if e = add(c, true); e != nil {
			return e
		}
	}
	var suffix []history.Change
	for _, c := range newc {
		if c.ID == common {
			break
		}
		suffix = append(suffix, c)
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		if e = add(suffix[i], false); e != nil {
			return e
		}
	}
	var paths []PathChange
	keys := make([]string, 0, len(changes))
	for p := range changes {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, p := range keys {
		c := changes[p]
		if e = current(p, c.Before); e != nil {
			return e
		}
		paths = append(paths, *c)
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	for _, c := range paths {
		if e = apply(c.Path, c.After); e != nil {
			return e
		}
	}
	cleanupDirs(paths)
	return m.Store.CommitRestore(session, target)
}
func cleanupDirs(changes []PathChange) {
	var dirs []string
	for _, c := range changes {
		if !c.After.Exists {
			dirs = append(dirs, c.CreatedDirs...)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, p := range dirs {
		_ = os.Remove(p)
	}
}

// Admit serializes a context/turn checkpoint with complete file mutations. The
// callback must not call Apply or Restore, which own the same gate.
func (m *Manager) Admit(ctx context.Context, fn func() error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}
