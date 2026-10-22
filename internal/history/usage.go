package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	ctxmgr "ttc/internal/context"
	"ttc/internal/llm"
)

// MeasuredRequest binds endpoint token counts to a selected immutable main
// coding completion. Messages and System are populated only for an older input
// without a Checkpoint; they exclude the response and all later arrivals.
type MeasuredRequest struct {
	RequestID  int64
	Selection  llm.Selection
	Tokens     llm.Usage
	Checkpoint *ctxmgr.Checkpoint
	Messages   []llm.Message
	System     string
}

// MainUsage separates the last successful completion's reported usage from the
// latest usable calibration anchor. A successful completion without usage clears
// Reported but preserves an older Anchor. Anchor never crosses compaction.
type MainUsage struct {
	Reported *MeasuredRequest
	Anchor   *MeasuredRequest
}

// Request observations are immutable projections written with completion events.
// Selecting them through physical ancestry/source identities avoids decoding audit
// status payloads or consulting mutable latest-request status and diagnostics.
const usageAncestry = `WITH RECURSIVE ancestry(id,parent_id) AS (
	SELECT id,parent_id FROM entries WHERE id=?
	UNION ALL SELECT e.id,e.parent_id FROM entries e JOIN ancestry a ON e.id=a.parent_id
) `

type requestObservation struct {
	request, admission, prompt int64 // Original request/admission/system-prompt identities, not copied row IDs.
	selection                  llm.Selection
	usage                      *llm.Usage // Nil means unavailable; a measured zero remains nonnil.
	checkpoint                 *ctxmgr.Checkpoint
}

// MainUsage reads typed main coding observations on the selected branch in one
// snapshot. Naming, compaction, children, siblings and later source completions
// are excluded. Checkpoints need no original-input reads. Older observations use
// their saved admission and prompt identities to reconstruct approximate input;
// a missing instruction artifact disables that anchor without failing the load.
func (s *Store) MainUsage(ctx context.Context, session string) (MainUsage, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return MainUsage{}, err
	}
	defer tx.Rollback()
	var tip, summary int64
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(active_entry_id,0) FROM sessions WHERE id=?", session).Scan(&tip); err != nil {
		return MainUsage{}, err
	}
	if err := tx.QueryRowContext(ctx, usageAncestry+`SELECT coalesce(max(coalesce(e.source_id,e.id)),0)
		FROM ancestry a CROSS JOIN entries e WHERE e.id=a.id AND e.kind='summary' AND e.actor_id='main'`, tip).Scan(&summary); err != nil {
		return MainUsage{}, err
	}
	latest, err := mainObservation(ctx, tx, tip, false)
	if err != nil {
		return MainUsage{}, err
	}
	var result MainUsage
	if latest != nil && latest.usage != nil {
		result.Reported = latest.measured()
	}
	anchor := latest
	if anchor != nil && anchor.usage == nil {
		anchor, err = mainObservation(ctx, tx, tip, true)
		if err != nil {
			return MainUsage{}, err
		}
	}
	if anchor != nil && anchor.usage != nil && anchor.admission > summary {
		result.Anchor, err = anchor.input(ctx, tx, tip)
		if err != nil {
			return MainUsage{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return MainUsage{}, err
	}
	return result, nil
}

func mainObservation(ctx context.Context, tx *sql.Tx, tip int64, measuredOnly bool) (*requestObservation, error) {
	var value requestObservation
	var model string
	var usage, checkpoint sql.NullString
	var completion int64
	err := tx.QueryRowContext(ctx, usageAncestry+`SELECT o.completion_entry_id
		FROM ancestry a CROSS JOIN entries e JOIN request_observations o ON o.completion_entry_id=coalesce(e.source_id,e.id)
		JOIN model_requests r ON r.id=o.request_id
		WHERE e.id=a.id AND r.actor_id='main' AND r.purpose='coding' AND o.status='completed'
		AND (?=0 OR o.usage_json IS NOT NULL) ORDER BY e.id DESC LIMIT 1`, tip, measuredOnly).Scan(&completion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read main request observation: %w", err)
	}
	// Acquire payloads only after selecting one identity; SQLite may otherwise
	// evaluate all historical projections before sorting the ancestry.
	err = tx.QueryRowContext(ctx, `SELECT o.request_id,coalesce(o.admission_entry_id,0),
		coalesce(o.system_prompt_entry_id,0),r.model_json,o.usage_json,o.checkpoint_json
		FROM request_observations o JOIN model_requests r ON r.id=o.request_id WHERE o.completion_entry_id=?`, completion).Scan(
		&value.request, &value.admission, &value.prompt, &model, &usage, &checkpoint)
	if err != nil {
		return nil, fmt.Errorf("read selected request observation: %w", err)
	}
	if !usage.Valid {
		return &value, nil
	}
	if err := json.Unmarshal([]byte(model), &value.selection); err != nil {
		return nil, fmt.Errorf("decode observed selection: %w", err)
	}
	if err := json.Unmarshal([]byte(usage.String), &value.usage); err != nil {
		return nil, fmt.Errorf("decode observed usage: %w", err)
	}
	if !value.usage.Usable() {
		return nil, fmt.Errorf("invalid measured usage for request %d", value.request)
	}
	if checkpoint.Valid {
		if err := json.Unmarshal([]byte(checkpoint.String), &value.checkpoint); err != nil {
			return nil, fmt.Errorf("decode observed input checkpoint: %w", err)
		}
		if value.checkpoint != nil {
			if err := value.checkpoint.Validate(); err != nil {
				return nil, fmt.Errorf("decode observed input checkpoint: %w", err)
			}
		}
	}
	return &value, nil
}

func (o requestObservation) measured() *MeasuredRequest {
	return &MeasuredRequest{RequestID: o.request, Selection: o.selection, Tokens: *o.usage, Checkpoint: o.checkpoint}
}

func (o requestObservation) input(ctx context.Context, tx *sql.Tx, tip int64) (*MeasuredRequest, error) {
	var selected bool
	if err := tx.QueryRowContext(ctx, usageAncestry+`SELECT EXISTS(
		SELECT 1 FROM ancestry a CROSS JOIN entries e WHERE e.id=a.id AND coalesce(e.source_id,e.id)=?)`, tip, o.admission).Scan(&selected); err != nil {
		return nil, err
	}
	if !selected {
		return nil, nil
	}
	var originalSession string
	err := tx.QueryRowContext(ctx, `SELECT e.session_id FROM entries e JOIN model_requests r ON r.id=?
		WHERE e.id=? AND e.source_id IS NULL AND e.session_id=r.session_id AND e.actor_id=r.actor_id`, o.request, o.admission).Scan(&originalSession)
	if err != nil {
		return nil, fmt.Errorf("read observed admission: %w", err)
	}
	measured := o.measured()
	if o.checkpoint != nil {
		return measured, nil
	}
	if o.prompt == 0 {
		return nil, nil
	}
	var path string
	if err := tx.QueryRowContext(ctx, "SELECT json_extract(content_json,'$.path') FROM entries WHERE id=?", o.prompt).Scan(&path); err != nil {
		return nil, fmt.Errorf("read observed system prompt: %w", err)
	}
	prompt, err := readArtifact(path, instructionSnapshotBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read observed system prompt: %w", err)
	}
	messages, err := messagesWith(tx, originalSession, o.admission)
	if err != nil {
		return nil, fmt.Errorf("reconstruct observed input: %w", err)
	}
	measured.Messages = llm.ContextFor(measured.Selection, messages)
	measured.System = string(prompt)
	return measured, nil
}
