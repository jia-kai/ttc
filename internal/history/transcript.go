package history

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"

	"sort"
	"strings"
	"ttc/internal/filelock"

	"ttc/internal/prompts"
)

// transcriptSnapshot owns a separate read-only WAL connection, so a slow output
// writer cannot occupy the Store's serialized commit connection. Its independent
// shared lifetime lock fences schema upgrades and VACUUM while the reader lives.
// Runtime activity protection, not this lock, retains the loaded lineage's files.
func (s *Store) transcriptSnapshot(ctx context.Context) (contextHistoryReader, func(), error) {
	lock, err := filelock.AcquireShared(ctx, filepath.Join(s.Root, "history-use.lock"))
	if err != nil {
		return contextHistoryReader{}, nil, err
	}
	uri := url.URL{Scheme: "file", Path: filepath.Join(s.Root, "history.sqlite"), RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		lock.Close()
		return contextHistoryReader{}, nil, err
	}
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		db.Close()
		lock.Close()
		return contextHistoryReader{}, nil, err
	}
	return contextHistoryReader{ctx, tx}, func() { tx.Rollback(); db.Close(); lock.Close() }, nil
}

// validateTranscriptTip accepts zero only as an explicitly empty branch.
func validateTranscriptTip(q historyReader, session string, tip int64) error {
	var found bool
	if tip < 0 {
		return errors.New(prompts.HistoryTranscriptNegativeTip)
	}
	if err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM sessions WHERE id=?) AND (?=0 OR EXISTS(SELECT 1 FROM entries WHERE id=? AND session_id=?))", session, tip, tip, session).Scan(&found); err != nil {
		return err
	}
	if !found {
		return errors.New(prompts.HistoryTranscriptTip)
	}
	return nil
}

type actorBoundary struct {
	Type    string `json:"type"`
	Session string `json:"source_session"`
	Tip     int64  `json:"source_tip"`
}

// RecordActorPrefix records an aside's frozen admitted main-history prefix.
// It does not change model input. The source cut must exist when recorded.
func (s *Store) RecordActorPrefix(session, turn, actor, sourceSession string, sourceTip int64) error {
	return s.recordActorBoundary(session, turn, actor, sourceSession, sourceTip, "actor_context_prefix")
}

// RecordActorCompaction records the frozen cut summarized by an isolated actor.
// It leaves main history and undo ownership unchanged.
func (s *Store) RecordActorCompaction(session, turn, actor, sourceSession string, sourceTip int64) error {
	return s.recordActorBoundary(session, turn, actor, sourceSession, sourceTip, "actor_compacted")
}

func (s *Store) recordActorBoundary(session, turn, actor, sourceSession string, sourceTip int64, kind string) error {
	if strings.TrimSpace(actor) == "" || actor == "main" {
		return errors.New(prompts.HistoryActorBoundary)
	}
	return s.transact(func(tx *sql.Tx) error {
		if err := validateTranscriptTip(tx, sourceSession, sourceTip); err != nil {
			return err
		}
		var valid bool
		if err := tx.QueryRow(`WITH RECURSIVE selected(id,parent_id,actor_id,kind) AS (
			SELECT e.id,e.parent_id,e.actor_id,e.kind FROM entries e JOIN sessions s ON s.active_entry_id=e.id WHERE s.id=?
			UNION ALL SELECT e.id,e.parent_id,e.actor_id,e.kind FROM entries e JOIN selected p ON e.id=p.parent_id
		) SELECT EXISTS(SELECT 1 FROM sessions a JOIN sessions b ON a.lineage_id=b.lineage_id AND a.workspace_id=b.workspace_id WHERE a.id=? AND b.id=?)
			AND (?=0 OR EXISTS(SELECT 1 FROM selected WHERE id=?))`, sourceSession, session, sourceSession, sourceTip, sourceTip).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return errors.New(prompts.HistoryTranscriptTip)
		}
		if kind == "actor_compacted" {
			entries, err := selectedActorEntries(context.Background(), tx, sourceSession, actor, sourceTip)
			if err != nil {
				return err
			}
			found := false
			for _, e := range entries {
				found = found || (e.Actor == actor && (e.Kind == "message" || e.Kind == "tool_call" || e.Kind == "tool_result"))
			}
			if !found {
				return errors.New(prompts.HistoryTranscriptTip)
			}
		}
		data, err := json.Marshal(actorBoundary{kind, sourceSession, sourceTip})
		if err != nil {
			return err
		}
		_, err = appendTx(tx, session, turn, actor, "status", "", false, data, 0)
		return err
	})
}

// selectedActorEntries follows only selected parent chains and summary cuts.
// Summary source identity also preserves ancestry through manual Load copies;
// no session-wide source scan can accidentally admit abandoned sibling branches.
func selectedActorEntries(ctx context.Context, q historyReader, session, actor string, tip int64) ([]Entry, error) {
	if strings.TrimSpace(actor) == "" {
		return nil, errors.New(prompts.HistoryTranscriptActor)
	}
	type cut struct {
		session, actor string
		tip            int64
	}
	queue := []cut{{session, actor, tip}}
	seen := map[cut]bool{}
	unique := map[int64]Entry{}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c := queue[0]
		queue = queue[1:]
		if seen[c] {
			continue
		}
		seen[c] = true
		branch, err := actorBranchWith(q, c.session, c.tip, c.actor, false)
		if err != nil {
			return nil, err
		}
		for _, e := range branch {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if e.Kind == "summary" {
				var previous string
				var sourceTip int64
				err := q.QueryRow("SELECT predecessor_id,source_tip_id FROM compactions WHERE summary_entry_id=?", e.ID).Scan(&previous, &sourceTip)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return nil, err
				}
				if err == nil {
					queue = append(queue, cut{previous, c.actor, sourceTip})
				}
			}
			if e.Actor != c.actor {
				continue
			}
			if e.Kind == "status" {
				var boundary actorBoundary
				if err := json.Unmarshal(e.Content, &boundary); err != nil {
					return nil, err
				}
				if boundary.Type == "actor_context_prefix" {
					queue = append(queue, cut{boundary.Session, "main", boundary.Tip})
				}
			}
			unique[e.EventSeq()] = e
		}
	}
	out := make([]Entry, 0, len(unique))
	for _, e := range unique {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EventSeq() < out[j].EventSeq() })
	return out, nil
}

// ActorTranscript renders one actor's selected history, including original
// precompaction ancestry and an aside's recorded main prefix. Tip is an explicit
// frozen physical entry ID in session; zero means empty, never the current tip.
func (s *Store) ActorTranscript(session, actor string, tip int64) ([]byte, error) {
	var out bytes.Buffer
	if err := s.WriteActorTranscript(context.Background(), &out, session, actor, tip, "markdown"); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ActorTranscriptJSONL exports exact original envelopes for ActorTranscript's cut.
func (s *Store) ActorTranscriptJSONL(session, actor string, tip int64) ([]byte, error) {
	var out bytes.Buffer
	if err := s.WriteActorTranscript(context.Background(), &out, session, actor, tip, "jsonl"); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// WriteActorTranscript writes Markdown or exact JSONL for an explicit frozen cut.
// Selection and rendering use a cancellable read snapshot. Selected actor
// envelopes are held in memory; rendering streams one entry at a time to w.
// The caller owns w, including exclusive creation and cleanup of its output.
func (s *Store) WriteActorTranscript(ctx context.Context, w io.Writer, session, actor string, tip int64, format string) error {
	if format != "markdown" && format != "jsonl" {
		return fmt.Errorf(prompts.HistoryTranscriptFormat, format)
	}
	q, closeSnapshot, err := s.transcriptSnapshot(ctx)
	if err != nil {
		return err
	}
	defer closeSnapshot()
	entries, err := selectedActorEntries(ctx, q, session, actor, tip)
	if err != nil {
		return err
	}
	if format == "jsonl" {
		err = s.writeTranscriptJSONLEntries(ctx, q, w, entries)
	} else {
		err = s.writeTranscriptEntries(ctx, q, w, "Conversation · "+actor, entries)
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}

// ActorCompactionTip returns the latest selected compaction's frozen source cut.
// It returns an error when the selected actor history has not been compacted.
func (s *Store) ActorCompactionTip(ctx context.Context, session, actor string, tip int64) (string, int64, error) {
	q, closeSnapshot, err := s.transcriptSnapshot(ctx)
	if err != nil {
		return "", 0, err
	}
	defer closeSnapshot()
	if strings.TrimSpace(actor) == "" {
		return "", 0, errors.New(prompts.HistoryTranscriptActor)
	}
	seen := map[int64]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		// Load only boundary records, not actor conversation payloads, and stop
		// at the nearest selected actor boundary rather than walking all history.
		entries, err := actorBranchWith(q, session, tip, actor, true)
		if err != nil {
			return "", 0, err
		}
		var summary, boundary Entry
		for _, e := range entries {
			if e.Kind == "summary" && e.EventSeq() > summary.EventSeq() {
				summary = e
			}
			if e.Actor == actor && e.Kind == "status" && e.EventSeq() > boundary.EventSeq() {
				boundary = e
			}
		}
		if actor != "main" && boundary.ID != 0 {
			var b actorBoundary
			if err := json.Unmarshal(boundary.Content, &b); err != nil {
				return "", 0, err
			}
			return b.Session, b.Tip, nil
		}
		if summary.ID == 0 {
			return "", 0, errors.New(prompts.HistoryTranscriptNoCompaction)
		}
		if seen[summary.ID] {
			return "", 0, errors.New(prompts.HistoryCompactionAncestry)
		}
		seen[summary.ID] = true
		if err := q.QueryRow("SELECT predecessor_id,source_tip_id FROM compactions WHERE summary_entry_id=?", summary.ID).Scan(&session, &tip); err != nil {
			return "", 0, err
		}
		if actor == "main" {
			return session, tip, nil
		}
	}
}
