package history

import (
	"encoding/json"
	"fmt"
	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/render"
)

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
		var finish ChildFinish
		if json.Unmarshal(v.Content, &finish) == nil && finish.Type == "child_turn_finished" {
			label := "Assignment " + finish.TurnID + " · " + render.Status(finish.Status)
			if finish.Error != "" {
				label += " · " + render.Clean(finish.Error)
			}
			return label
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
				return fmt.Sprintf(prompts.HistoryRequestMessageLabel, x.Purpose, x.Role)
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
			if m.Runtime && m.Role == "developer" {
				return "Runtime context · inspect"
			}
			return m.Role + " · " + render.Clean(m.DisplayText())
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
