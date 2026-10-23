package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"ttc/internal/llm"
	"ttc/internal/prompts"
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
	if data.Status.Type == "presentation_stream" {
		data.PromptText, err = s.boundaryGroupAll(ctx, id)
		if err != nil {
			return Inspection{}, err
		}
	}
	inspection, err = projectInspection(data)
	if data.Status.Type == "presentation_stream" {
		var record boundaryRecord
		if decodeErr := json.Unmarshal(entry.Content, &record); decodeErr != nil {
			return Inspection{}, decodeErr
		}
		inspection.Structured = record.ReadableVersion > 0
	}
	return inspection, err
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

// RequestMessages atomically records an ordered internal request input batch as
// hidden request_message status entries attributed to the owning request actor.
// Turn and purpose must match the request; session must be its owning session or
// a same-workspace, same-lineage continuation. Canonical messages are saved
// unchanged, with inspection roles derived from Message.Role. Recording does not
// add coding context or acknowledge runtime events. IDs follow input order; errors
// return no IDs and leave history unchanged. An empty batch validates ownership
// without adding entries.
func (s *Store) RequestMessages(session, turn, purpose string, request int64, messages []llm.Message) ([]int64, error) {
	ids := make([]int64, 0, len(messages))
	err := s.transact(func(tx *sql.Tx) error {
		var ownerSession, ownerTurn, actor, ownerPurpose string
		if err := tx.QueryRow("SELECT session_id,coalesce(turn_id,''),actor_id,purpose FROM model_requests WHERE id=?", request).Scan(&ownerSession, &ownerTurn, &actor, &ownerPurpose); err != nil {
			return err
		}
		if turn != ownerTurn || purpose != ownerPurpose {
			return fmt.Errorf(prompts.HistoryRequestMessageOwner, request)
		}
		if session != ownerSession {
			// In-flight child requests retain their owner when main compaction
			// moves subsequent records into the writable continuation.
			var successor bool
			if err := tx.QueryRow(`WITH RECURSIVE successors(id,workspace_id,lineage_id) AS (
				SELECT id,workspace_id,lineage_id FROM sessions WHERE id=? UNION ALL
				SELECT s.id,s.workspace_id,s.lineage_id FROM sessions s JOIN successors p
				ON s.predecessor_id=p.id AND s.workspace_id=p.workspace_id AND s.lineage_id=p.lineage_id
			) SELECT EXISTS(SELECT 1 FROM successors WHERE id=?)`, ownerSession, session).Scan(&successor); err != nil {
				return err
			}
			if !successor {
				return fmt.Errorf(prompts.HistoryRequestMessageSession, request)
			}
		}
		for i, message := range messages {
			if err := message.Validate(); err != nil {
				return fmt.Errorf(prompts.HistoryRequestMessage, i, err)
			}
			data, err := json.Marshal(map[string]any{"type": "request_message", "purpose": purpose, "request_id": request, "role": message.Role(), "message": message})
			if err != nil {
				return fmt.Errorf(prompts.HistoryRequestMessage, i, err)
			}
			id, err := appendTx(tx, session, turn, actor, "status", "", false, data, 0)
			if err != nil {
				return fmt.Errorf(prompts.HistoryRequestMessage, i, err)
			}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}
