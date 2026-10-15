package history

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"ttc/internal/llm"
	"ttc/internal/privatefile"
	"ttc/internal/prompts"
	"ttc/internal/render"
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
	return s.transcriptEntries(v.Name, entries)
}

func (s *Store) transcriptEntries(name string, entries []Entry) ([]byte, error) {
	return s.transcriptEntriesContext(context.Background(), name, entries)
}

func (s *Store) transcriptEntriesContext(ctx context.Context, name string, entries []Entry) ([]byte, error) {
	var out bytes.Buffer
	if err := s.writeTranscriptEntries(ctx, s.DB, &out, name, entries); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (s *Store) writeTranscriptEntries(ctx context.Context, q historyReader, out io.Writer, name string, entries []Entry) error {
	completed := map[string]bool{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Kind == "tool_result" {
			var ref struct {
				CallID string `json:"call_id"`
			}
			if err := json.Unmarshal(entry.Content, &ref); err != nil {
				return err
			}
			completed[ref.CallID] = true
		}
	}
	if _, err := fmt.Fprintf(out, "# %s\n\n", render.Inline(name)); err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Kind == "tool_call" {
			var ref struct {
				CallID string `json:"call_id"`
			}
			if err := json.Unmarshal(entry.Content, &ref); err != nil {
				return err
			}
			if completed[ref.CallID] {
				continue
			}
		}
		text, err := s.exportTextWith(ctx, q, entry)
		if err != nil {
			return err
		}
		if text == "" {
			continue
		}
		kind := entry.Role
		if kind == "" {
			kind = entry.Kind
		}
		if _, err := fmt.Fprintf(out, "### %s · %s · #%d\n\n%s\n\n", render.Inline(entry.Actor), render.Inline(kind), entry.ID, text); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// ExportText renders the same inspectable content without metadata duplication.
// System instructions have no export presentation. Tool records may override
// their inspector presentation with a compact export body.
func (s *Store) ExportText(entry Entry) (string, error) {
	return s.exportTextWith(context.Background(), s.DB, entry)
}

func (s *Store) exportTextWith(ctx context.Context, q historyReader, entry Entry) (string, error) {
	if entry.InternalEvent() {
		return "", nil
	}
	if entry.Kind == "status" {
		// Payload fields vary by status type; only decode the discriminator here.
		var status struct{ Type string }
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
		var message llm.Message
		if err := json.Unmarshal(entry.Content, &message); err != nil {
			return "", err
		}
		if json.Valid([]byte(message.Content)) {
			message.Content = render.Fence(message.Content, "json")
		}
		text := render.Clean(message.Content)
		for _, snapshot := range message.Files {
			path := strings.ReplaceAll(render.Clean(snapshot.Path), ">", "\\>")
			link := fmt.Sprintf("Binary file: [snapshot](<%s>)", path)
			// Dimensions are optional presentation metadata; the durable path and
			// exact reference remain available even if image decoding is unavailable.
			if f, err := os.OpenFile(snapshot.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
				if info, err := f.Stat(); err == nil && info.Mode().IsRegular() {
					if config, _, err := image.DecodeConfig(io.LimitReader(f, 1<<20)); err == nil {
						link += fmt.Sprintf(" · %d×%d", config.Width, config.Height)
					}
				}
				f.Close()
			}
			if text != "" {
				text += "\n\n"
			}
			text += link
		}
		return text, nil
	}
	if entry.Kind == "tool_call" {
		var ref struct {
			CallID string `json:"call_id"`
		}
		if err := json.Unmarshal(entry.Content, &ref); err != nil {
			return "", err
		}
		var call, name string
		if err := q.QueryRow("SELECT name,call_json FROM tool_calls WHERE id=?", ref.CallID).Scan(&name, &call); err != nil {
			return "", err
		}
		return "Pending call:\n\n" + render.Tool(name, json.RawMessage(call), nil).Detail, nil
	}
	if entry.Kind == "tool_result" {
		var raw string
		if err := q.QueryRow("SELECT markdown_json FROM tool_records WHERE entry_id=?", entry.ID).Scan(&raw); err != nil {
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
	return s.transcriptJSONLEntries(entries)
}

func (s *Store) transcriptJSONLEntries(entries []Entry) ([]byte, error) {
	return s.transcriptJSONLEntriesContext(context.Background(), entries)
}

func (s *Store) transcriptJSONLEntriesContext(ctx context.Context, entries []Entry) ([]byte, error) {
	var out bytes.Buffer
	if err := s.writeTranscriptJSONLEntries(ctx, s.DB, &out, entries); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (s *Store) writeTranscriptJSONLEntries(ctx context.Context, q historyReader, out io.Writer, entries []Entry) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		record := map[string]any{"entry": entry, "event_seq": entry.EventSeq()}
		var status struct{ Type, Path string }
		if entry.Kind == "status" && json.Unmarshal(entry.Content, &status) == nil && status.Type == "system_prompt" {
			prompt, err := readArtifact(status.Path, instructionSnapshotBytes)
			if err != nil {
				return err
			}
			record["instructions"] = string(prompt)
		}
		if entry.Kind == "tool_call" || entry.Kind == "tool_result" {
			var ref struct {
				CallID string `json:"call_id"`
			}
			if err := json.Unmarshal(entry.Content, &ref); err != nil {
				return err
			}
			var call, providerID, name string
			var request int64
			var version int
			if err := q.QueryRow("SELECT call_json,request_id,call_version,provider_call_id,name FROM tool_calls WHERE id=?", ref.CallID).Scan(&call, &request, &version, &providerID, &name); err != nil {
				return err
			}
			record["call"] = map[string]any{"id": ref.CallID, "request_id": request, "version": version, "provider_call_id": providerID, "name": name, "payload": json.RawMessage(call)}
			if entry.Kind == "tool_result" {
				var raw, md string
				if err := q.QueryRow("SELECT record_json,markdown_json FROM tool_records WHERE entry_id=?", entry.ID).Scan(&raw, &md); err != nil {
					return err
				}
				record["tool_record"], record["presentation"] = json.RawMessage(raw), json.RawMessage(md)
			}
		}
		if err := enc.Encode(record); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// ArchiveActorTranscript freezes an actor's complete selected conversation in a
// private, immutable Markdown/JSONL pair under the session lineage. It includes
// original pre-compaction entries and previous assignments, not just summaries
// or the current model input. Copies are deduplicated by source event identity;
// sibling branches and unrelated actors are excluded. The returned path names
// Markdown; its exact JSONL companion is path + ".jsonl". A nonnil pending
// assistant message captures output that could not be committed to SQLite. It
// is appended as an explicitly uncommitted actor/message record, without a
// fabricated history entry ID or completion.
func (s *Store) ArchiveActorTranscript(session, actor string, pending *llm.Message) (string, error) {
	if strings.TrimSpace(actor) == "" {
		return "", fmt.Errorf(prompts.HistoryTranscriptActor)
	}
	if pending != nil && pending.Type != llm.TypeAssistant {
		return "", fmt.Errorf(prompts.HistoryTranscriptPendingRole)
	}
	v, err := s.Session(session)
	if err != nil {
		return "", err
	}
	entries, err := selectedActorEntries(context.Background(), s.DB, session, actor, v.EntryTip)
	if err != nil {
		return "", err
	}
	text, err := s.transcriptEntries(fmt.Sprintf("Child conversation · %s", actor), entries)
	if err != nil {
		return "", err
	}
	exact, err := s.transcriptJSONLEntries(entries)
	if err != nil {
		return "", err
	}
	if pending != nil {
		captured, err := archiveMessageJSONL(actor, []llm.Message{*pending}, true)
		if err != nil {
			return "", err
		}
		exact = append(exact, captured...)
		body, err := json.Marshal(pending)
		if err != nil {
			return "", err
		}
		presentation, err := s.ExportText(Entry{Kind: "message", Content: body})
		if err != nil {
			return "", err
		}
		text = append(text, []byte(fmt.Sprintf("### Uncommitted assistant output\n\n%s\n\n", presentation))...)
	}
	return s.writeArchive(session, text, exact)
}

func actorBranchWith(q historyReader, session string, tip int64, actor string, boundariesOnly bool) ([]Entry, error) {
	if err := validateTranscriptTip(q, session, tip); err != nil {
		return nil, err
	}
	// Actor filtering must follow, not constrain, recursion: another actor's row
	// can connect two selected actor entries. Keep payloads out of the recursive
	// work table so unrelated messages and opaque state are never materialized.
	rows, err := q.Query(`WITH RECURSIVE ancestry(id,parent_id) AS (
		SELECT id,parent_id FROM entries WHERE id=? AND session_id=?
		UNION ALL
		SELECT e.id,e.parent_id FROM entries e JOIN ancestry a ON e.id=a.parent_id
	) SELECT e.id,coalesce(e.parent_id,0),coalesce(e.source_id,0),coalesce(e.file_tip_id,0),e.session_id,coalesce(e.turn_id,''),e.actor_id,e.kind,coalesce(e.role,''),e.model_visible,e.content_json,e.created_ms,coalesce(e.main_turn_id,''),coalesce(e.undo_owner_turn_id,'')
	FROM ancestry a JOIN entries selected ON selected.id=a.id JOIN entries e ON e.id=coalesce(selected.source_id,selected.id) WHERE (e.actor_id=? OR e.kind='summary') AND (?=0 OR e.kind='summary' OR (e.kind='status' AND json_extract(e.content_json,'$.type')='actor_compacted'))`, tip, session, actor, boundariesOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var entry Entry
		var content string
		if err := rows.Scan(&entry.ID, &entry.Parent, &entry.Source, &entry.FileTip, &entry.SessionID, &entry.TurnID, &entry.Actor, &entry.Kind, &entry.Role, &entry.Visible, &content, &entry.CreatedMS, &entry.MainTurnID, &entry.UndoOwnerTurnID); err != nil {
			return nil, err
		}
		entry.Content = json.RawMessage(content)
		out = append(out, entry)
	}
	return out, rows.Err()
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
			return privatefile.PrivateDir(target)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("unsupported managed export asset: %s", source)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		raw, err := readArtifact(source, info.Size())
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
		return privatefile.AtomicFile(target, raw, 0600)
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
