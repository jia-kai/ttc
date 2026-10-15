package history

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"ttc/internal/llm"
	"ttc/internal/render"
)

// ChildResponse resolves a foreground subagent result or runtime completion
// message to its full committed assistant response. Zero means no reference is
// available (including failed/interrupted responses without a committed entry).
// References must remain within the envelope's lineage and producing child.
// Loaded sessions retain references to immutable entries in their source session.
// The original entry remains independently inspectable for completion metadata.
func (s *Store) ChildResponse(id int64) (int64, error) {
	entry, err := s.Entry(id)
	if err != nil {
		return 0, err
	}
	var raw []byte
	if entry.Kind == "tool_result" {
		var result string
		err := s.DB.QueryRow(`SELECT c.result_json FROM tool_records r JOIN tool_calls c ON c.id=r.call_id WHERE r.entry_id=? AND c.name='subagent'`, id).Scan(&result)
		if err == sql.ErrNoRows {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		raw = []byte(result)
	} else if entry.Kind == "message" && entry.Role == "user" {
		var message llm.Message
		if err := json.Unmarshal(entry.Content, &message); err != nil {
			return 0, err
		}
		if message.Type != llm.TypeRuntimeNotification {
			return 0, nil
		}
		var notice struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(message.Content), &notice) != nil || notice.Type != "child_turn_finished" {
			return 0, nil
		}
		raw = []byte(message.Content)
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
	target, err := s.Entry(reference.ResultEntry)
	if err != nil {
		return 0, fmt.Errorf("child response reference: %w", err)
	}
	var sameLineage bool
	if err := s.DB.QueryRow(`SELECT a.lineage_id=b.lineage_id AND a.workspace_id=b.workspace_id FROM sessions a JOIN sessions b ON b.id=? WHERE a.id=?`, target.SessionID, entry.SessionID).Scan(&sameLineage); err != nil {
		return 0, err
	}
	if !strings.HasPrefix(reference.ChildID, "main/child_") || !sameLineage || target.Actor != reference.ChildID || target.Kind != "message" || target.Role != "assistant" {
		return 0, fmt.Errorf("invalid child response reference %d from entry %d", target.ID, id)
	}
	return target.ID, nil
}

// InternalEvent identifies durable ordering and delivery records whose effects
// are already presented by messages or tool results. They remain inspectable in
// the history tree and exact JSONL transcript, but do not add conversation rows.
func (v Entry) InternalEvent() bool {
	if v.Kind != "status" {
		return false
	}
	var status struct{ Type string }
	if json.Unmarshal(v.Content, &status) != nil {
		return false
	}
	switch status.Type {
	case "request_admitted", "request_finished", "job_state", "timer_state", "child_state", "turn_finished", "runtime_event", "compaction_started", "compaction_recovery", "file_changed":
		return true
	}
	return false
}

// Entry loads one node for inspection, including hidden system/status records.
func (s *Store) Entry(id int64) (Entry, error) {
	var v Entry
	var b string
	e := s.DB.QueryRow(`SELECT id,coalesce(parent_id,0),coalesce(source_id,0),coalesce(file_tip_id,0),session_id,coalesce(turn_id,''),actor_id,kind,coalesce(role,''),model_visible,content_json,created_ms,coalesce(main_turn_id,''),coalesce(undo_owner_turn_id,'') FROM entries WHERE id=?`, id).Scan(&v.ID, &v.Parent, &v.Source, &v.FileTip, &v.SessionID, &v.TurnID, &v.Actor, &v.Kind, &v.Role, &v.Visible, &b, &v.CreatedMS, &v.MainTurnID, &v.UndoOwnerTurnID)
	v.Content = json.RawMessage(b)
	return v, e
}

// Label returns a short view title without actor decoration, keeping system
// prompt contents behind a placeholder. Frontends own actor attribution styles.
func (s *Store) Label(v Entry) string {
	if v.Kind == "tool_result" {
		var raw string
		if e := s.DB.QueryRow("SELECT markdown_json FROM tool_records WHERE entry_id=?", v.ID).Scan(&raw); e == nil {
			var md render.Markdown
			if json.Unmarshal([]byte(raw), &md) == nil {
				return md.Summary
			}
		}
	}

	if v.Kind == "status" {
		var marker struct{ Type string }
		if json.Unmarshal(v.Content, &marker) == nil && marker.Type == "child_turn_finished" {
			if finish, err := readChildFinish(v.Content); err == nil {
				label := "Assignment " + finish.TurnID + " · " + render.Status(finish.Outcome)
				if finish.Error != "" {
					label += " · " + render.Clean(finish.Error)
				}
				return label
			}
		}
		var job struct {
			Type     string
			Markdown render.Markdown
		}
		if json.Unmarshal(v.Content, &job) == nil && job.Type == "job_completion" {
			return job.Markdown.Summary
		}
		var x struct{ Type, Text, Label, Purpose, Role string }
		if json.Unmarshal(v.Content, &x) == nil {
			if x.Type == "request_message" {
				return fmt.Sprintf("%s %s · inspect", x.Purpose, x.Role)
			}
			if x.Type == "system_prompt" {
				return "System prompt · inspect"
			}
			if x.Text != "" {
				return render.Clean(x.Text)
			}
			if v.InternalEvent() {
				return render.Clean(x.Type)
			}
		}
	}
	if v.Visible || v.Kind == "message" {
		var m llm.Message
		if json.Unmarshal(v.Content, &m) == nil {
			switch m.Type.Presentation() {
			case llm.PresentationContext:
				return "Runtime context · inspect"
			case llm.PresentationInstruction:
				if m.Type == llm.TypeRuntimeInstruction {
					return "Runtime instructions · inspect"
				}
				return "Instructions · inspect"
			case llm.PresentationNotification:
				return "Runtime notification · inspect"
			case llm.PresentationGuidance:
				return "Child guidance · inspect"
			}
			return m.Role() + " · " + render.Clean(m.DisplayText())
		}
	}
	if v.Actor != "main" {
		return v.Kind
	}
	return v.Actor + " · " + v.Kind
}

// ValidateArchive verifies both required files at handoff/load boundaries.
// Ordinary metadata reads avoid repeatedly hashing immutable private archives.
func (s *Store) ValidateArchive(session string) error {
	rows, e := s.DB.Query("SELECT archive_path,archive_sha256,archive_exact_sha256 FROM compactions WHERE continuation_id=?", session)
	if e != nil {
		return e
	}
	defer rows.Close()
	for rows.Next() {
		var path, hash, exactHash string
		if e = rows.Scan(&path, &hash, &exactHash); e != nil {
			return e
		}
		for _, file := range []struct{ path, hash string }{{path, hash}, {path + ".jsonl", exactHash}} {
			actual, err := filepathHash(file.path)
			if err != nil {
				return fmt.Errorf("required compaction archive %s is missing or unreadable: %w", file.path, err)
			}
			if actual != file.hash || file.hash == "" {
				return fmt.Errorf("required compaction archive %s is corrupt", file.path)
			}
		}
	}
	return rows.Err()
}

// RequestMessage records internal naming/summary messages outside the coding
// context, attributed to the owning request actor. Purpose controls inspection.
func (s *Store) RequestMessage(session, turn, purpose, role string, request int64, message llm.Message) (int64, error) {
	var actor string
	if err := s.DB.QueryRow("SELECT actor_id FROM model_requests WHERE id=?", request).Scan(&actor); err != nil {
		return 0, err
	}
	return s.Append(session, turn, actor, "status", "", false, map[string]any{"type": "request_message", "purpose": purpose, "request_id": request, "role": role, "message": message})
}
