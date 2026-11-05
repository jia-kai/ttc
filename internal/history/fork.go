package history

import (
	"encoding/json"
	"errors"
	"fmt"

	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// forkBaseWith reads the immutable manual-fork cut. Zero denotes no inherited
// entries, including a fork whose frozen prefix is empty. Unknown sessions fail.
func forkBaseWith(q historyReader, session string) (int64, error) {
	var base int64
	err := q.QueryRow("SELECT coalesce(base_entry_id,0) FROM sessions WHERE id=?", session).Scan(&base)
	return base, err
}

// validateHistoryTip accepts an explicitly empty cut, every locally owned row,
// and only the frozen base's ancestry. Same-lineage source siblings and later
// source arrivals are not members. Current trusted tips need not call it again.
func validateHistoryTip(q historyReader, session string, tip int64) error {
	if tip < 0 {
		return errors.New(prompts.HistoryTranscriptNegativeTip)
	}
	var base int64
	var local bool
	if err := q.QueryRow(`SELECT coalesce(base_entry_id,0),EXISTS(SELECT 1 FROM entries WHERE id=? AND session_id=?) FROM sessions WHERE id=?`, tip, session, session).Scan(&base, &local); err != nil {
		return err
	}
	if tip == 0 || local || tip == base {
		return nil
	}
	var inherited bool
	if base != 0 && tip < base {
		if err := q.QueryRow(`WITH RECURSIVE inherited(id,parent_id) AS (
			SELECT id,parent_id FROM entries WHERE id=?
			UNION ALL SELECT e.id,e.parent_id FROM inherited a CROSS JOIN entries e
			WHERE e.id=a.parent_id AND e.id<a.id AND a.id>?
		) SELECT EXISTS(SELECT 1 FROM inherited WHERE id=?)`, base, tip, tip).Scan(&inherited); err != nil {
			return err
		}
	}
	if !inherited {
		return errors.New(prompts.HistoryTranscriptTip)
	}
	return nil
}

// projectForkEntry applies selection-local visibility/file baselines, never
// changing physical provenance or canonical content. Parent traversal is already
// restricted to a valid logical cut before calling this helper.
func projectForkEntry(session string, entry *Entry) error {
	if entry.SessionID == session {
		return nil
	}
	entry.FileTip = 0
	if entry.Kind == "message" || entry.Kind == "summary" {
		var message llm.Message
		if err := json.Unmarshal(entry.Content, &message); err != nil {
			return fmt.Errorf(prompts.HistoryDecodeCanonicalMessage, entry.Kind, err)
		}
		if message.Role() != entry.Role {
			return fmt.Errorf(prompts.HistoryMessageIndexedRole, message.Type, message.Role(), entry.Role)
		}
		if entry.Kind == "message" && message.Type.IsRuntime() {
			entry.Visible = false
		}
	}
	return nil
}
