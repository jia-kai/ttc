package history

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"scicode/internal/provider"
	"scicode/internal/render"
	"strings"
)

// Transcript freezes the selected branch as dense, user-readable Markdown.
// Exact envelopes/provider payloads belong in TranscriptJSONL, not this view.
// Tool intents with a result in the selected cut are represented by that result.
func (s *Store) Transcript(session string, tip int64) ([]byte, error) {
	v, err := s.Session(session)
	if err != nil {
		return nil, err
	}
	entries, err := s.Branch(session, tip)
	if err != nil {
		return nil, err
	}
	completed := map[string]bool{}
	for _, entry := range entries {
		if entry.Kind == "tool_result" {
			var ref struct {
				CallID string `json:"call_id"`
			}
			if err := json.Unmarshal(entry.Content, &ref); err != nil {
				return nil, err
			}
			completed[ref.CallID] = true
		}
	}
	var out strings.Builder
	out.WriteString("# " + render.Inline(v.Name) + "\n\n")
	for _, entry := range entries {
		if entry.Kind == "tool_call" {
			var ref struct {
				CallID string `json:"call_id"`
			}
			if err := json.Unmarshal(entry.Content, &ref); err != nil {
				return nil, err
			}
			if completed[ref.CallID] {
				continue
			}
		}
		text, err := s.ExportText(entry)
		if err != nil {
			return nil, err
		}
		if text == "" {
			continue
		}
		kind := entry.Role
		if kind == "" {
			kind = entry.Kind
		}
		fmt.Fprintf(&out, "### %s · %s · #%d\n\n%s\n\n", render.Inline(entry.Actor), render.Inline(kind), entry.ID, text)
	}
	return []byte(out.String()), nil
}

// ExportText renders the same inspectable content without metadata duplication.
// System instructions have no export presentation. Tool records may override
// their inspector presentation with a compact export body.
func (s *Store) ExportText(entry Entry) (string, error) {
	var status struct {
		Type    string
		Message provider.Message
	}
	if entry.Kind == "status" {
		if err := json.Unmarshal(entry.Content, &status); err != nil {
			return "", err
		}
		if status.Type == "system_prompt" {
			return "", nil
		}
		if status.Type == "request_message" {
			return render.Clean(s.Label(entry)), nil
		}
	}
	if entry.Kind == "message" || entry.Kind == "summary" {
		var message provider.Message
		if err := json.Unmarshal(entry.Content, &message); err != nil {
			return "", err
		}
		if json.Valid([]byte(message.Content)) {
			return render.Fence(message.Content, "json"), nil
		}
		return render.Clean(message.Content), nil
	}
	if entry.Kind == "tool_call" {
		var ref struct {
			CallID string `json:"call_id"`
		}
		if err := json.Unmarshal(entry.Content, &ref); err != nil {
			return "", err
		}
		var call, name string
		if err := s.DB.QueryRow("SELECT name,call_json FROM tool_calls WHERE id=?", ref.CallID).Scan(&name, &call); err != nil {
			return "", err
		}
		return "Pending call:\n\n" + render.Tool(name, json.RawMessage(call), nil).Detail, nil
	}
	if entry.Kind == "tool_result" {
		var raw string
		if err := s.DB.QueryRow("SELECT markdown_json FROM tool_records WHERE entry_id=?", entry.ID).Scan(&raw); err != nil {
			return "", err
		}
		var md render.Markdown
		if err := json.Unmarshal([]byte(raw), &md); err != nil {
			return "", err
		}
		return md.ExportText(), nil
	}
	return s.Inspect(entry)
}

// TranscriptJSONL saves original entry envelopes and exact related tool records,
// including instruction text and opaque provider state, through the selected cut.
// Pending calls never acquire a result committed after that cut.
func (s *Store) TranscriptJSONL(session string, tip int64) ([]byte, error) {
	entries, err := s.Branch(session, tip)
	if err != nil {
		return nil, err
	}
	var out strings.Builder
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	for _, entry := range entries {
		record := map[string]any{"entry": entry}
		var status struct{ Type, Path string }
		if entry.Kind == "status" && json.Unmarshal(entry.Content, &status) == nil && status.Type == "system_prompt" {
			prompt, err := os.ReadFile(status.Path)
			if err != nil {
				return nil, err
			}
			record["instructions"] = string(prompt)
		}
		if entry.Kind == "tool_call" || entry.Kind == "tool_result" {
			var ref struct {
				CallID string `json:"call_id"`
			}
			if err := json.Unmarshal(entry.Content, &ref); err != nil {
				return nil, err
			}
			var call string
			if err := s.DB.QueryRow("SELECT call_json FROM tool_calls WHERE id=?", ref.CallID).Scan(&call); err != nil {
				return nil, err
			}
			var request int64
			var version int
			var providerID, name string
			if err := s.DB.QueryRow("SELECT request_id,call_version,provider_call_id,name FROM tool_calls WHERE id=?", ref.CallID).Scan(&request, &version, &providerID, &name); err != nil {
				return nil, err
			}
			record["call"] = map[string]any{"id": ref.CallID, "request_id": request, "version": version, "provider_call_id": providerID, "name": name, "payload": json.RawMessage(call)}
			if entry.Kind == "tool_result" {
				var raw, md string
				if err := s.DB.QueryRow("SELECT record_json,markdown_json FROM tool_records WHERE entry_id=?", entry.ID).Scan(&raw, &md); err != nil {
					return nil, err
				}
				record["tool_record"], record["presentation"] = json.RawMessage(raw), json.RawMessage(md)
			}
		}
		if err := enc.Encode(record); err != nil {
			return nil, err
		}
	}
	return []byte(out.String()), nil
}

// ArchiveTranscript writes the same Markdown/JSONL pair used by /export under
// the lineage, deduplicating immutable Markdown by content hash.
func (s *Store) ArchiveTranscript(session string, tip int64) (string, error) {
	text, err := s.Transcript(session, tip)
	if err != nil {
		return "", err
	}
	exact, err := s.TranscriptJSONL(session, tip)
	if err != nil {
		return "", err
	}
	v, err := s.Session(session)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append(append([]byte(nil), text...), exact...))
	dir := filepath.Join(s.Root, "lineages", v.LineageID, "compactions")
	if err := PrivateDir(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%x.md", sum))
	for _, file := range []struct {
		path string
		data []byte
	}{{path, text}, {path + ".jsonl", exact}} {
		if old, err := os.ReadFile(file.path); err == nil {
			if !bytes.Equal(old, file.data) {
				return "", errors.New("archive content conflict")
			}
			continue
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if err := AtomicFile(file.path, file.data, 0600); err != nil {
			return "", err
		}
	}

	return path, nil
}

// Export rejects existing targets and copies referenced snapshots into a sibling assets directory.
func (s *Store) Export(session, path string) error {
	path, e := filepath.Abs(path)
	if e != nil {
		return e
	}
	if _, e = os.Lstat(path); e == nil {
		return fmt.Errorf("export target exists")
	} else if !os.IsNotExist(e) {
		return e
	}
	sidecar := path + ".jsonl"
	if _, e = os.Lstat(sidecar); e == nil {
		return fmt.Errorf("export JSONL target exists")
	} else if !os.IsNotExist(e) {
		return e
	}
	assets := path + ".assets"
	if _, e = os.Lstat(assets); e == nil {
		return fmt.Errorf("export assets target exists")
	} else if !os.IsNotExist(e) {
		return e
	}
	v, e := s.Session(session)
	if e != nil {
		return e
	}
	data, e := s.Transcript(session, v.EntryTip)
	if e != nil {
		return e
	}
	exact, e := s.TranscriptJSONL(session, v.EntryTip)
	if e != nil {
		return e
	}
	if e = os.Mkdir(assets, 0700); e != nil {
		return e
	}
	ok := false
	sidecarCreated := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(assets)
			if sidecarCreated {
				_ = os.Remove(sidecar)
			}
		}
	}()
	lineage := filepath.Join(s.Root, "lineages", v.LineageID)
	e = filepath.WalkDir(lineage, func(source string, d fs.DirEntry, err error) error {
		if os.IsNotExist(err) && source == lineage {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(lineage, source)
		if err != nil {
			return err
		}
		target := filepath.Join(assets, rel)
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in managed export assets: %s", source)
		}
		if d.IsDir() {
			return PrivateDir(target)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("unsupported managed export asset: %s", source)
		}
		raw, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		// Rewrite portable archive Markdown only. Exact JSONL, instructions and
		// source snapshots retain their original bytes.
		if strings.HasPrefix(filepath.ToSlash(rel), "compactions/") && !strings.HasSuffix(rel, ".jsonl") {
			relativeRoot, err := filepath.Rel(filepath.Dir(target), assets)
			if err != nil {
				return err
			}
			raw = []byte(strings.ReplaceAll(string(raw), lineage+string(filepath.Separator), relativeRoot+"/"))
		}
		return AtomicFile(target, raw, 0600)
	})
	if e != nil {
		return e
	}
	data = []byte(strings.ReplaceAll(string(data), lineage+string(filepath.Separator), filepath.Base(assets)+"/"))
	sf, e := os.OpenFile(sidecar, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	sidecarCreated = true
	_, e = sf.Write(exact)
	if e == nil {
		e = sf.Sync()
	}
	if closeErr := sf.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(data)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		os.Remove(path)
		return e
	}
	ok = true
	return nil
}
