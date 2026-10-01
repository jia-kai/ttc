package history

import (
	"encoding/json"
	"errors"
	"scicode/internal/provider"
	"scicode/internal/render"
)

// Entry loads one node for inspection, including hidden system/status records.
func (s *Store) Entry(id int64) (Entry, error) {
	var v Entry
	var b string
	e := s.DB.QueryRow(`SELECT id,coalesce(parent_id,0),coalesce(source_id,0),coalesce(file_tip_id,0),session_id,coalesce(turn_id,''),actor_id,kind,coalesce(role,''),model_visible,content_json,created_ms FROM entries WHERE id=?`, id).Scan(&v.ID, &v.Parent, &v.Source, &v.FileTip, &v.SessionID, &v.TurnID, &v.Actor, &v.Kind, &v.Role, &v.Visible, &b, &v.CreatedMS)
	v.Content = json.RawMessage(b)
	return v, e
}

// Label returns a short view title while keeping system-prompt contents behind a placeholder.
func (s *Store) Label(v Entry) (label string) {
	markdown := v.Kind == "tool_result"
	defer func() {
		if v.Actor != "main" {
			actor := render.Clean(v.Actor)
			if markdown {
				actor = render.Inline(v.Actor)
			}
			label = actor + " · " + label
		}
	}()
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
		var job struct {
			Type     string
			Markdown render.Markdown
		}
		if json.Unmarshal(v.Content, &job) == nil && job.Type == "job_completion" {
			markdown = true
			return job.Markdown.Summary
		}
		var x struct{ Type, Text, Label, Purpose, Role string }
		if json.Unmarshal(v.Content, &x) == nil {
			if x.Type == "request_message" {
				return x.Purpose + " " + x.Role + " · inspect"
			}
			if x.Type == "system_prompt" {
				return "System prompt · inspect"
			}
			if x.Text != "" {
				return render.Clean(x.Text)
			}
		}
	}
	if v.Visible || v.Kind == "message" {
		var m provider.Message
		if json.Unmarshal(v.Content, &m) == nil {
			if m.Runtime && m.Role == "developer" {
				return "Runtime context · inspect"
			}
			return m.Role + " · " + render.Clean(m.Content)
		}
	}
	if v.Actor != "main" {
		return v.Kind
	}
	return v.Actor + " · " + v.Kind
}

// ValidateArchive checks a managed compaction reference before history is projected.
func (s *Store) ValidateArchive(session string) error {
	rows, e := s.DB.Query("SELECT archive_path,archive_sha256 FROM compactions WHERE continuation_id=?", session)
	if e != nil {
		return e
	}
	defer rows.Close()
	for rows.Next() {
		var path, hash string
		if e = rows.Scan(&path, &hash); e != nil {
			return e
		}
		if filepathHash(path) != hash || hash == "" {
			return errors.New("required compaction archive is missing or corrupt")
		}
	}
	return rows.Err()
}

// RequestMessage records internal naming/summary messages outside the coding context.
func (s *Store) RequestMessage(session, turn, purpose, role string, request int64, message provider.Message) (int64, error) {
	return s.Append(session, turn, "main", "status", "", false, map[string]any{"type": "request_message", "purpose": purpose, "request_id": request, "role": role, "message": message})
}
