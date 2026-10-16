package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"ttc/internal/llm"
	"ttc/internal/render"
)

// contextSQLReader is implemented by both the commit DB and frozen read Tx.
type contextSQLReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// contextHistoryReader makes the shared acquisition helpers cancellable without
// replacing the caller's connection or escaping its read snapshot.
type contextHistoryReader struct {
	ctx    context.Context
	reader contextSQLReader
}

func (q contextHistoryReader) Query(query string, args ...any) (*sql.Rows, error) {
	return q.reader.QueryContext(q.ctx, query, args...)
}
func (q contextHistoryReader) QueryRow(query string, args ...any) *sql.Row {
	return q.reader.QueryRowContext(q.ctx, query, args...)
}

// ChildResponse resolves a foreground subagent result or runtime completion
// message to its full committed assistant response. Zero means no reference is
// available (including failed/interrupted responses without a committed entry).
// References must remain within the envelope's lineage and producing child.
// Loaded sessions retain references to immutable entries in their source session.
// The original entry remains independently inspectable for completion metadata.
func (s *Store) ChildResponse(id int64) (int64, error) {
	// This lookup runs before paged inspection. Keep unrelated messages and the
	// referenced assistant's potentially large replay envelope out of Go memory.
	var entry Entry
	entry.ID = id
	var messageType string
	if err := s.db.QueryRow(`SELECT session_id,kind,coalesce(role,''),
		CASE WHEN kind='message' AND role='user' THEN coalesce((
			SELECT value FROM json_each(entries.content_json) AS field
			WHERE lower(field.key)='type' AND field.type='text' ORDER BY field.id DESC LIMIT 1
		),'') ELSE '' END
		FROM entries WHERE id=?`, id).Scan(&entry.SessionID, &entry.Kind, &entry.Role, &messageType); err != nil {
		return 0, err
	}
	var raw []byte
	if entry.Kind == "tool_result" {
		var result string
		err := s.db.QueryRow(`SELECT c.result_json FROM tool_records r JOIN tool_calls c ON c.id=r.call_id WHERE r.entry_id=? AND c.name='subagent'`, id).Scan(&result)
		if err == sql.ErrNoRows {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		raw = []byte(result)
	} else if messageType == string(llm.TypeRuntimeNotification) {
		var content string
		// Append validates with Go's canonical decoder, which accepts case-folded
		// field names and uses the last non-null string among duplicate keys.
		// Match those semantics without loading the complete message envelope.
		if err := s.db.QueryRow(`SELECT coalesce((
			SELECT value FROM json_each(entries.content_json) AS field
			WHERE lower(field.key)='content' AND field.type='text' ORDER BY field.id DESC LIMIT 1
		),'') FROM entries WHERE id=?`, id).Scan(&content); err != nil {
			return 0, err
		}
		var notice struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(content), &notice) != nil || notice.Type != "child_turn_finished" {
			return 0, nil
		}
		raw = []byte(content)
	} else {
		return 0, nil
	}
	var reference struct {
		ChildID     string `json:"child_id"`
		ResultEntry int64  `json:"result_entry_id"`
	}
	if err := json.Unmarshal(raw, &reference); err != nil {
		return 0, err
	}
	if reference.ResultEntry == 0 {
		return 0, nil
	}
	var target Entry
	target.ID = reference.ResultEntry
	if err := s.db.QueryRow("SELECT session_id,actor_id,kind,coalesce(role,'') FROM entries WHERE id=?", target.ID).Scan(&target.SessionID, &target.Actor, &target.Kind, &target.Role); err != nil {
		return 0, fmt.Errorf("child response reference: %w", err)
	}
	var sameLineage bool
	if err := s.db.QueryRow(`SELECT a.lineage_id=b.lineage_id AND a.workspace_id=b.workspace_id FROM sessions a JOIN sessions b ON b.id=? WHERE a.id=?`, target.SessionID, entry.SessionID).Scan(&sameLineage); err != nil {
		return 0, err
	}
	if !strings.HasPrefix(reference.ChildID, "main/child_") || !sameLineage || target.Actor != reference.ChildID || target.Kind != "message" || target.Role != "assistant" {
		return 0, fmt.Errorf("invalid child response reference %d from entry %d", target.ID, id)
	}
	return target.ID, nil
}

// Entry loads one node for inspection, including hidden system/status records.
func (s *Store) Entry(id int64) (Entry, error) {
	return entryWith(s.db, id)
}

func entryWith(q historyReader, id int64) (Entry, error) {
	var v Entry
	var b string
	err := q.QueryRow(`SELECT id,coalesce(parent_id,0),coalesce(source_id,0),coalesce(file_tip_id,0),session_id,coalesce(turn_id,''),actor_id,kind,coalesce(role,''),model_visible,content_json,created_ms,coalesce(main_turn_id,''),coalesce(undo_owner_turn_id,'') FROM entries WHERE id=?`, id).Scan(&v.ID, &v.Parent, &v.Source, &v.FileTip, &v.SessionID, &v.TurnID, &v.Actor, &v.Kind, &v.Role, &v.Visible, &b, &v.CreatedMS, &v.MainTurnID, &v.UndoOwnerTurnID)
	v.Content = json.RawMessage(b)
	return v, err
}

// ReadView decodes a supplied entry for frontend replay, loading only tool summary
// and prompt-purpose metadata. It never loads tool details or instruction files.
func (s *Store) ReadView(ctx context.Context, entry Entry) (view EntryView, err error) {
	defer func() { err = intentError(fmt.Sprintf("read entry #%d view", entry.ID), err) }()
	if err := ctx.Err(); err != nil {
		return EntryView{}, err
	}
	data, err := decodePresentation(entry)
	if err != nil {
		return EntryView{}, err
	}
	q := contextHistoryReader{ctx, s.db}
	if entry.Kind == "tool_result" {
		var summary string
		if err := q.QueryRow("SELECT coalesce(json_extract(markdown_json,'$.summary'),'') FROM tool_records WHERE entry_id=?", entry.ID).Scan(&summary); err != nil {
			return EntryView{}, err
		}
		data.ToolSummary = &summary
	}
	if data.Prompt != nil && data.Prompt.RequestID > 0 {
		if err := q.QueryRow("SELECT purpose FROM model_requests WHERE id=?", data.Prompt.RequestID).Scan(&data.Prompt.Purpose); err != nil {
			return EntryView{}, err
		}
	}
	return projectView(data), ctx.Err()
}

// ReadEntryView loads an entry by ID and decodes its small replay view with
// cancellation. Like ReadView, it does not load tool details or instruction files.
// Unknown entries return ErrNotFound.
func (s *Store) ReadEntryView(ctx context.Context, id int64) (EntryView, error) {
	entry, err := entryWith(contextHistoryReader{ctx, s.db}, id)
	if err != nil {
		return EntryView{}, intentError(fmt.Sprintf("read entry #%d view", id), err)
	}
	return s.ReadView(ctx, entry)
}

// ReadInspection loads and projects one complete small inspector. Call InspectPage
// first for potentially large records; this method deliberately loads full data.
// Unknown entries or missing required related rows return ErrNotFound.
func (s *Store) ReadInspection(ctx context.Context, id int64) (inspection Inspection, err error) {
	defer func() { err = intentError(fmt.Sprintf("read entry #%d inspection", id), err) }()
	q := contextHistoryReader{ctx, s.db}
	entry, err := entryWith(q, id)
	if err != nil {
		return Inspection{}, err
	}
	data, err := loadInspectionPresentation(ctx, q, entry)
	if err != nil {
		return Inspection{}, err
	}
	return projectInspection(data)
}

func loadInspectionPresentation(ctx context.Context, q historyReader, entry Entry) (entryPresentation, error) {
	data, err := decodePresentation(entry)
	if err != nil {
		return data, err
	}
	data, err = decodeInspectionBody(data)
	if err != nil {
		return data, err
	}
	if entry.Kind == "tool_result" {
		md, err := toolMarkdownWith(q, entry.ID)
		if err != nil {
			return data, err
		}
		data.Markdown, data.ToolSummary = md, &md.Summary
	}
	if entry.Kind == "tool_call" {
		var ref struct {
			CallID string `json:"call_id"`
		}
		if err := json.Unmarshal(entry.Content, &ref); err != nil {
			return data, err
		}
		var args, name string
		var result sql.NullString
		if err := q.QueryRow("SELECT name,call_json,result_json FROM tool_calls WHERE id=?", ref.CallID).Scan(&name, &args, &result); err != nil {
			return data, err
		}
		data.Call = &callPresentation{Name: name, Arguments: json.RawMessage(args), Result: json.RawMessage(result.String)}
	}
	if data.Prompt != nil {
		text, _, err := instructionPage(ctx, data.Prompt.Path, 0, instructionSnapshotBytes)
		if err != nil {
			return data, err
		}
		data.PromptText = text
	}
	return data, ctx.Err()
}

func toolMarkdownWith(q historyReader, id int64) (*render.Markdown, error) {
	var raw string
	if err := q.QueryRow("SELECT markdown_json FROM tool_records WHERE entry_id=?", id).Scan(&raw); err != nil {
		return nil, err
	}
	var md render.Markdown
	if err := json.Unmarshal([]byte(raw), &md); err != nil {
		return nil, err
	}
	return &md, nil
}

// RequestMessage records internal naming/summary messages outside the coding
// context, attributed to the owning request actor. Purpose controls inspection.
func (s *Store) RequestMessage(session, turn, purpose, role string, request int64, message llm.Message) (int64, error) {
	var actor string
	if err := s.db.QueryRow("SELECT actor_id FROM model_requests WHERE id=?", request).Scan(&actor); err != nil {
		return 0, err
	}
	return s.Append(session, turn, actor, "status", "", false, map[string]any{"type": "request_message", "purpose": purpose, "request_id": request, "role": role, "message": message})
}
