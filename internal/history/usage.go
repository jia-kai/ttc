package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"ttc/internal/llm"
)

// MeasuredRequest binds endpoint token counts to a selected immutable main
// coding completion. Checkpoint is optional: legacy usage remains readable
// without reconstructing the original request input.
type MeasuredRequest struct {
	RequestID  int64
	Selection  llm.Selection
	Tokens     llm.Usage
	Checkpoint *llm.TokenCheckpoint
}

// MainUsage separates the last successful completion's reported usage from the
// latest owned measurement eligible for input accounting. A successful completion
// without usage clears Reported but preserves an older Anchor. Anchor never
// crosses compaction; callers decide whether its checkpoint is usable by a provider.
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
	request, admission int64 // Original request/admission identities, not copied row IDs.
	selection          llm.Selection
	usage              *llm.Usage // Nil means unavailable; a measured zero remains nonnil.
	checkpoint         *llm.TokenCheckpoint
}

// MainUsage reads typed main coding observations on the selected branch in one
// snapshot. Naming, compaction, children, siblings and later source completions
// are excluded. Observations need no original-input or prompt-artifact reads;
// anchors are returned only after validating selected admission ownership.
func (s *Store) MainUsage(ctx context.Context, session string) (MainUsage, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return MainUsage{}, err
	}
	defer tx.Rollback()
	var tip int64
	if err := tx.QueryRowContext(ctx, "SELECT coalesce(active_entry_id,0) FROM sessions WHERE id=?", session).Scan(&tip); err != nil {
		return MainUsage{}, err
	}
	result, err := mainUsageAt(ctx, tx, tip)
	if err != nil {
		return MainUsage{}, err
	}
	if err := tx.Commit(); err != nil {
		return MainUsage{}, err
	}
	return result, nil
}

func mainUsageAt(ctx context.Context, tx contextSQLReader, tip int64) (MainUsage, error) {
	var summary int64
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
	return result, nil
}

func mainObservation(ctx context.Context, tx contextSQLReader, tip int64, measuredOnly bool) (*requestObservation, error) {
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
		r.model_json,o.usage_json,o.checkpoint_json
		FROM request_observations o JOIN model_requests r ON r.id=o.request_id WHERE o.completion_entry_id=?`, completion).Scan(
		&value.request, &value.admission, &model, &usage, &checkpoint)
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

func (o requestObservation) input(ctx context.Context, tx contextSQLReader, tip int64) (*MeasuredRequest, error) {
	var selected bool
	if err := tx.QueryRowContext(ctx, usageAncestry+`SELECT EXISTS(
		SELECT 1 FROM ancestry a CROSS JOIN entries e WHERE e.id=a.id AND coalesce(e.source_id,e.id)=?)`, tip, o.admission).Scan(&selected); err != nil {
		return nil, err
	}
	if !selected {
		return nil, nil
	}
	var owned bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM entries e JOIN model_requests r ON r.id=?
		WHERE e.id=? AND e.source_id IS NULL AND e.session_id=r.session_id AND e.actor_id=r.actor_id)`, o.request, o.admission).Scan(&owned); err != nil {
		return nil, fmt.Errorf("read observed admission: %w", err)
	}
	if !owned {
		return nil, fmt.Errorf("observed admission %d does not own request %d", o.admission, o.request)
	}
	return o.measured(), nil
}
