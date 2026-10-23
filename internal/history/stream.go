package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"ttc/internal/llm"
	"ttc/internal/messagestream"
)

const boundaryArtifactBytes int64 = 256 << 20

// ErrBoundaryBaseChanged means preparation became stale after routing or the
// nearest selected request base changed. Prepare again outside publication locks.
var ErrBoundaryBaseChanged = errors.New("prepared boundary base changed")

// boundaryRecord retains capture artifacts separately from model context.
// Header identifies the request attempt; response/outcome entries are private
// revisions of its single presentation row.
type boundaryRecord struct {
	Type            string             `json:"type"`
	Item            messagestream.Item `json:"item"`
	Direction       string             `json:"direction"`
	Request         int64              `json:"request"`
	Attempt         int                `json:"attempt"`
	Purpose         string             `json:"purpose"`
	Conversation    string             `json:"conversation"`
	Raw             string             `json:"raw"`
	Readable        string             `json:"readable"`
	Expanded        string             `json:"expanded,omitempty"`
	JSON            string             `json:"json,omitempty"`
	ReadableVersion int                `json:"readable_version,omitempty"`
	Items           string             `json:"items,omitempty"` // Complete ordered immutable item references.
	Base            *boundaryBase      `json:"base,omitempty"`
	DeltaEmpty      bool               `json:"delta_empty,omitempty"`
}

// RecordBoundary prepares artifacts outside publication locks and publishes one
// new request row, or a revision updating the existing request attempt row.
func (s *Store) RecordBoundary(session, turn, actor string, request int64, conversation string, event llm.BoundaryEvent) ([]messagestream.Item, error) {
	for attempt := 0; attempt < 3; attempt++ {
		p, err := s.PrepareBoundary(session, request, event)
		if err != nil {
			return nil, err
		}
		items, err := s.RecordPreparedBoundary(session, turn, actor, request, conversation, p)
		if !errors.Is(err, ErrBoundaryBaseChanged) {
			return items, err
		}
	}
	return nil, ErrBoundaryBaseChanged
}

const boundaryAncestry = `WITH RECURSIVE ancestry(id,parent_id) AS (
 SELECT id,parent_id FROM entries WHERE id=?
 UNION ALL SELECT e.id,e.parent_id FROM entries e JOIN ancestry a ON e.id=a.parent_id) `

const boundaryHeaderMatch = `e.kind='status' AND json_extract(e.content_json,'$.type')='presentation_stream'
 AND json_extract(e.content_json,'$.direction')='request'
 AND json_extract(e.content_json,'$.request')=(SELECT request FROM target)
 AND json_extract(e.content_json,'$.attempt')=(SELECT attempt FROM target)`

// Stop as soon as the attempt's header is reached. A response immediately after
// its header must not walk or sort the complete conversation prefix.
const boundaryAttemptAncestry = `WITH RECURSIVE target(request,attempt,floor) AS (VALUES(?,?,?)), ancestry(id,parent_id,header) AS (
 SELECT e.id,e.parent_id,coalesce(` + boundaryHeaderMatch + `,0) FROM entries e WHERE e.id=? AND e.id>(SELECT floor FROM target)
 UNION ALL SELECT e.id,e.parent_id,coalesce(` + boundaryHeaderMatch + `,0)
 FROM entries e JOIN ancestry a ON e.id=a.parent_id WHERE NOT a.header AND e.id>(SELECT floor FROM target)) `

// RecordPreparedBoundary publishes on the selected writable continuation. Only
// headers on its ancestry may own a response, never a sibling branch's header.
func (s *Store) RecordPreparedBoundary(session, turn, actor string, request int64, conversation string, p PreparedBoundary) ([]messagestream.Item, error) {
	if p.raw == "" {
		return nil, errors.New("boundary was not prepared")
	}
	if request != p.request || actor != p.actor {
		return nil, ErrBoundaryBaseChanged
	}
	var item messagestream.Item
	err := s.transact(func(tx *sql.Tx) error {
		var lineage string
		var tip int64
		if err := tx.QueryRow("SELECT lineage_id,coalesce(active_entry_id,0) FROM sessions WHERE id=?", session).Scan(&lineage, &tip); err != nil {
			return err
		}
		if lineage != p.lineage {
			return ErrBoundaryBaseChanged
		}
		var ownerSession, ownerTurn, ownerActor, purpose string
		var cutoff int64
		if err := tx.QueryRow("SELECT session_id,coalesce(turn_id,''),actor_id,purpose,event_cutoff FROM model_requests WHERE id=?", request).Scan(&ownerSession, &ownerTurn, &ownerActor, &purpose, &cutoff); err != nil {
			return err
		}
		if actor != ownerActor || turn != ownerTurn {
			return errors.New("boundary request actor/turn mismatch")
		}
		var successor bool
		if err := tx.QueryRow(`WITH RECURSIVE successors(id) AS (SELECT id FROM sessions WHERE id=? UNION ALL SELECT s.id FROM sessions s JOIN successors p ON s.predecessor_id=p.id) SELECT EXISTS(SELECT 1 FROM successors WHERE id=?)`, ownerSession, session).Scan(&successor); err != nil {
			return err
		}
		if !successor {
			return errors.New("boundary request session mismatch")
		}
		var header int64
		err := tx.QueryRow(boundaryAttemptAncestry+`SELECT id FROM ancestry WHERE header LIMIT 1`, request, p.attempt, cutoff, tip).Scan(&header)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if p.direction == "request" && header != 0 {
			return errors.New("duplicate boundary request attempt")
		}
		if p.direction != "request" && header == 0 {
			return errors.New("boundary detail has no request header on selected ancestry")
		}
		state := "running"
		if p.direction == "outcome" {
			state = p.state
			if state == "" {
				state = "finished"
			}
		}
		item = messagestream.Item{ID: header, Actor: actor, Title: fmt.Sprintf("Request #%d · %s · attempt %d · %s", request, purpose, p.attempt, state)}
		r := boundaryRecord{Type: "presentation_stream", Item: item, Direction: p.direction, Request: request, Attempt: p.attempt, Purpose: purpose, Conversation: conversation, Raw: p.raw, Readable: p.readable, Expanded: p.expanded, JSON: p.prettyJSON, ReadableVersion: p.readableVersion}
		if p.direction == "request" && p.itemRefs != "" {
			var nearest int64
			err := tx.QueryRow(boundaryNearestBase+`SELECT id FROM ancestry WHERE matched LIMIT 1`, actor, purpose, tip).Scan(&nearest)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if nearest != p.baseID || purpose != p.purpose {
				return ErrBoundaryBaseChanged
			}
			r.Items, r.Base, r.DeltaEmpty = p.itemRefs, p.base, p.deltaEmpty
		}
		data, err := json.Marshal(r)
		if err != nil {
			return err
		}
		id, err := appendTx(tx, session, turn, actor, "status", "", false, data, 0)
		if err != nil {
			return err
		}
		if header == 0 {
			header = id
		}
		item.ID = header
		item.Revision = id
		item.Actions = boundaryActions(id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return []messagestream.Item{item}, nil
}
func boundaryActions(revision int64) []messagestream.Action {
	return []messagestream.Action{{Kind: messagestream.Inspect, Target: revision}, {Kind: messagestream.Raw, Target: revision}}
}

// StreamItems replays the latest revision per request attempt on selected
// ancestry, preserving header order. Old per-part change entries are ignored.
func (s *Store) StreamItems(ctx context.Context, session string) ([]messagestream.Item, error) {
	var tip int64
	if err := s.db.QueryRowContext(ctx, "SELECT coalesce(active_entry_id,0) FROM sessions WHERE id=?", session).Scan(&tip); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, boundaryAncestry+`SELECT e.id,e.actor_id,e.content_json FROM ancestry a JOIN entries e ON e.id=a.id WHERE e.kind='status' AND json_extract(e.content_json,'$.type')='presentation_stream' ORDER BY e.id`, tip)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []messagestream.Item{}
	positions := map[string]int{}
	for rows.Next() {
		var id int64
		var actor string
		var raw []byte
		if err := rows.Scan(&id, &actor, &raw); err != nil {
			return nil, err
		}
		var r boundaryRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		if r.Direction == "change" {
			continue
		}
		key := fmt.Sprintf("%d/%d", r.Request, r.Attempt)
		pos, exists := positions[key]
		if r.Direction == "request" {
			if exists {
				continue
			}
			pos = len(items)
			positions[key] = pos
			items = append(items, r.Item)
			items[pos].ID = id
		} else if !exists {
			continue
		}
		header := items[pos].ID
		items[pos] = r.Item
		items[pos].ID = header
		items[pos].Actor = actor
		items[pos].Revision = id
		items[pos].Actions = boundaryActions(id)
	}
	return items, rows.Err()
}

// BoundaryRaw returns exact saved provider bytes for one boundary event.
func (s *Store) BoundaryRaw(ctx context.Context, id int64) ([]byte, error) {
	var path string
	if err := s.db.QueryRowContext(ctx, `SELECT json_extract(content_json,'$.raw') FROM entries WHERE id=? AND kind='status' AND json_extract(content_json,'$.type')='presentation_stream'`, id).Scan(&path); err != nil {
		return nil, err
	}
	return readBoundaryAll(ctx, path)
}

// BoundaryRawPage pages exact provider bytes without decoding the capture.
func (s *Store) BoundaryRawPage(ctx context.Context, id int64, offset, limit int) (InspectionPage, error) {
	page := InspectionPage{ID: id, Offset: offset, Limit: limit, JSON: true, Supported: true}
	if id <= 0 || offset < 0 || limit < 1 || limit > InspectionPageChars {
		return page, errors.New("invalid boundary raw page")
	}
	var path string
	if err := s.db.QueryRowContext(ctx, `SELECT actor_id,json_extract(content_json,'$.item.title'),json_extract(content_json,'$.raw') FROM entries WHERE id=? AND kind='status' AND json_extract(content_json,'$.type')='presentation_stream'`, id).Scan(&page.Actor, &page.Title, &path); err != nil {
		return page, err
	}
	text, total, err := boundaryTextPage(ctx, path, offset, limit)
	page.Text, page.Total = text, total
	return page, err
}
