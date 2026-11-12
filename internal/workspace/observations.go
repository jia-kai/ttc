package workspace

import (
	"context"
	"path/filepath"
)

// contentFingerprint distinguishes an absent file from an existing empty file.
// Permission-only changes do not affect full-rewrite observations.
type contentFingerprint struct {
	exists bool
	hash   string
}

func fingerprint(s State) contentFingerprint {
	return contentFingerprint{exists: s.Exists, hash: s.Hash}
}

type fileKnowledge struct {
	revision          uint64 // Advances whenever current knowledge changes or a tracked mutation applies.
	current           contentFingerprint
	known             bool
	untracked         bool
	modifiedByActor   bool
	soleModifier      string
	multipleModifiers bool
	observed          map[string]contentFingerprint
}

func (m *Manager) fileKnowledge(path string) *fileKnowledge {
	if m.files == nil {
		m.files = make(map[string]*fileKnowledge)
	}
	k := m.files[path]
	if k == nil {
		k = &fileKnowledge{observed: make(map[string]contentFingerprint)}
		m.files[path] = k
	}
	return k
}

func (k *fileKnowledge) reconcile(current contentFingerprint) {
	if !k.known || k.current != current {
		k.revision++
	}
	if k.known && k.current != current {
		// Shells, restores and external processes have no attributable actor.
		// Keep that uncertainty even after another actor reads or edits the file.
		k.untracked = true
		// An untracked divergence requires a fresh read, including when bytes
		// happen to match an older observation predating the known tracked state.
		clear(k.observed)
	}
	k.current, k.known = current, true
}

func (k *fileKnowledge) allowWrite(actor string, current contentFingerprint) bool {
	if observed, ok := k.observed[actor]; ok && observed == current {
		return true
	}
	if k.untracked || k.multipleModifiers {
		return false
	}
	return !k.modifiedByActor || k.soleModifier == actor
}

func (k *fileKnowledge) modified(actor string, current contentFingerprint) {
	k.revision++
	k.current, k.known = current, true
	// Only the distinction between zero, one, and multiple modifiers matters:
	// once two actors modify a path, every actor has an "other" modifier.
	if !k.modifiedByActor {
		k.soleModifier, k.modifiedByActor = actor, true
	} else if k.soleModifier != actor {
		k.multipleModifiers = true
	}
}

// ObserveRead records a file-tool read without holding the mutation queue while
// reading or hashing. read must return the SHA-256 of the full stream used to
// produce the successful result, even for a
// partial page; an empty hash denotes a directory, which is not observed. The
// snapshot may predate concurrent mutations; its fingerprint is still the actor's
// observation, but cannot replace newer tracked state. Observations survive
// compaction for this Manager's lifetime, but are not restored from history.
func (m *Manager) ObserveRead(ctx context.Context, actor, path string, read func() (string, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path = m.Path(path)
	// Reads permit symlinks; mutations do not. Credit a read through an alias to
	// its resolved target rather than to an otherwise unwritable alias path.
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	var revision uint64
	if k := m.files[resolved]; k != nil {
		revision = k.revision
	}
	m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	checksum, err := read()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if checksum != "" {
		m.mu.Lock()
		defer m.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		current := contentFingerprint{exists: true, hash: checksum}
		k := m.fileKnowledge(resolved)
		if k.revision == revision {
			k.reconcile(current)
		}
		k.observed[actor] = current
	}
	return nil
}
