package session

import (
	"database/sql"
	"fmt"
	"time"

	"scicode/internal/history"
)

// RefreshInstructions records current fixed instructions when a writable
// session's active main coding snapshot is at least one hour old. It returns
// the new entry ID, or zero when no refresh is needed, without inference.
// Call before replacing a live runtime so failure leaves that runtime usable.
// Every subsequent request uses the same current template; project and live
// environment state are sampled separately at each boundary, with runtime
// context appended only when changed.
func RefreshInstructions(store *history.Store, saved history.Session) (int64, error) {
	if saved.ReadOnly || saved.EntryTip == 0 {
		return 0, nil
	}
	var createdMS int64
	err := store.DB.QueryRow(`WITH RECURSIVE branch AS (
		SELECT id,parent_id,actor_id,kind,content_json,created_ms FROM entries WHERE id=? AND session_id=?
		UNION ALL SELECT e.id,e.parent_id,e.actor_id,e.kind,e.content_json,e.created_ms FROM entries e JOIN branch b ON e.id=b.parent_id
	) SELECT b.created_ms FROM branch b LEFT JOIN model_requests q ON q.id=json_extract(b.content_json,'$.request_id')
	WHERE b.actor_id='main' AND b.kind='status' AND json_extract(b.content_json,'$.type')='system_prompt'
	AND (q.purpose='coding' OR coalesce(json_extract(b.content_json,'$.request_id'),0)=0)
	ORDER BY b.id DESC LIMIT 1`, saved.EntryTip, saved.ID).Scan(&createdMS)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("check saved instructions: %w", err)
	}
	if time.Since(time.UnixMilli(createdMS)) < time.Hour {
		return 0, nil
	}
	id, err := store.RecordSystemPrompt(saved.ID, "", "main", 0, systemTemplate)
	if err != nil {
		return 0, fmt.Errorf("refresh instructions: %w", err)
	}
	return id, nil
}
