package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"scicode/internal/provider"
	"scicode/internal/render"
)

// BranchNode is bounded display metadata for one immutable history entry.
// ID zero represents the empty session baseline. Parent identifies the tree edge;
// Selected marks the active ancestry, and Reason explains inspection-only nodes.
type BranchNode struct {
	ID, Parent        int64
	Actor, Kind, Role string
	Label             string
	Selected          bool
	UserInput         bool // An admitted human user/steer message, excluding runtime notices.
	Restorable        bool
	Reason            string
}

// BranchTree snapshots every branch of one session without loading message bodies
// or tool output. Nodes are ordered by entry ID, with the baseline first.
type BranchTree struct {
	SessionID, Name string
	Current         int64
	Nodes           []BranchNode
}

// HistoryTree reads bounded labels and structural metadata in one read transaction.
// It rejects corrupt parent links and checks pending main tools for each branch.
// Inspection remains available for read-only history and before an undo boundary.
func (s *Store) HistoryTree(ctx context.Context, session string) (BranchTree, error) {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return BranchTree{}, err
	}
	defer tx.Rollback()
	var tree BranchTree
	var readOnly bool
	var floor int64
	var invalid string
	if err = tx.QueryRowContext(ctx, "SELECT id,name,coalesce(active_entry_id,0),read_only,coalesce(undo_floor_id,0),coalesce(json_extract(metadata_json,'$.compaction_error'),'') FROM sessions WHERE id=?", session).Scan(&tree.SessionID, &tree.Name, &tree.Current, &readOnly, &floor, &invalid); err != nil {
		return BranchTree{}, err
	}
	tree.Nodes = []BranchNode{{Label: "Start of session", Restorable: !readOnly && invalid == "" && floor == 0}}
	rows, err := tx.QueryContext(ctx, `SELECT e.id,coalesce(e.parent_id,0),e.actor_id,e.kind,coalesce(e.role,''),
		substr(coalesce(CASE WHEN e.kind='tool_result' THEN json_extract(r.markdown_json,'$.summary')
		WHEN e.kind='tool_call' THEN c.name || ' · call'
		WHEN e.kind='status' AND json_extract(e.content_json,'$.type')='system_prompt' THEN 'System prompt'
		WHEN e.role='user' AND coalesce(json_extract(e.content_json,'$.runtime'),0)=0 THEN coalesce(json_extract(e.content_json,'$.user_text'),json_extract(e.content_json,'$.content'))
		ELSE coalesce(json_extract(e.content_json,'$.content'),json_extract(e.content_json,'$.text'),json_extract(e.content_json,'$.type')) END,e.kind),1,192),
		CASE WHEN e.model_visible=1 AND e.role='assistant' THEN coalesce(json_array_length(e.content_json,'$.calls'),0)
		WHEN e.model_visible=1 AND e.kind='tool_result' THEN -1 ELSE 0 END,
		(e.kind='message' AND e.role='user' AND e.actor_id='main' AND e.model_visible=1
		AND coalesce(json_extract(e.content_json,'$.runtime'),0)=0 AND coalesce(t.trigger IN ('user','steer'),0))
		FROM entries e LEFT JOIN tool_records r ON r.entry_id=e.id
		LEFT JOIN tool_calls c ON c.id=json_extract(e.content_json,'$.call_id')
		LEFT JOIN turns t ON t.id=e.turn_id
		WHERE e.session_id=? ORDER BY e.id`, session)
	if err != nil {
		return BranchTree{}, err
	}
	indices := map[int64]int{0: 0}
	pending := map[int64]int{0: 0}
	aboveFloor := map[int64]bool{0: floor == 0}
	for rows.Next() {
		var node BranchNode
		var delta int
		if err = rows.Scan(&node.ID, &node.Parent, &node.Actor, &node.Kind, &node.Role, &node.Label, &delta, &node.UserInput); err != nil {
			rows.Close()
			return BranchTree{}, err
		}
		if _, found := indices[node.Parent]; !found || node.Parent >= node.ID {
			rows.Close()
			return BranchTree{}, fmt.Errorf("invalid history parent #%d for #%d", node.Parent, node.ID)
		}
		pending[node.ID] = pending[node.Parent] + delta
		aboveFloor[node.ID] = node.ID == floor || aboveFloor[node.Parent]
		if pending[node.ID] < 0 {
			rows.Close()
			return BranchTree{}, fmt.Errorf("unpaired main tool result at #%d", node.ID)
		}
		node.Label = strings.Join(strings.Fields(render.Clean(node.Label)), " ")
		switch {
		case invalid != "":
			node.Reason = "context invalid after compaction failure"
		case readOnly:
			node.Reason = "read-only session"
		case !aboveFloor[node.ID]:
			node.Reason = "crosses undo boundary"
		case pending[node.ID] > 0:
			node.Reason = "pending main tool results"
		default:
			node.Restorable = true
		}
		indices[node.ID] = len(tree.Nodes)
		tree.Nodes = append(tree.Nodes, node)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return BranchTree{}, err
	}
	if err = rows.Close(); err != nil {
		return BranchTree{}, err
	}
	if !tree.Nodes[0].Restorable {
		if invalid != "" {
			tree.Nodes[0].Reason = "context invalid after compaction failure"
		} else if readOnly {
			tree.Nodes[0].Reason = "read-only session"
		} else {
			tree.Nodes[0].Reason = "before undo boundary"
		}
	}
	for id := tree.Current; ; id = tree.Nodes[indices[id]].Parent {
		index, found := indices[id]
		if !found {
			return BranchTree{}, fmt.Errorf("selected history entry #%d is missing", id)
		}
		tree.Nodes[index].Selected = true
		if id == 0 {
			break
		}
	}
	return tree, tx.Commit()
}

// BranchSelectionTarget resolves an explicit entry ID and rejects cuts inside
// unresolved main tool calls. It never changes history, files or live runtime state.
func (s *Store) BranchSelectionTarget(session string, id int64) (RestoreTarget, error) {
	if id < 0 {
		return RestoreTarget{}, errors.New("branch entry ID must be nonnegative")
	}
	saved, err := s.Session(session)
	if err != nil {
		return RestoreTarget{}, err
	}
	if saved.CompactionError != "" {
		return RestoreTarget{}, fmt.Errorf("context invalid after compaction failure: %s; select another session", saved.CompactionError)
	}
	target, err := s.BranchTarget(session, id)
	if err != nil || id == 0 {
		return target, err
	}
	entries, err := s.Branch(session, id)
	if err != nil {
		return RestoreTarget{}, err
	}
	var floor int64
	if err = s.DB.QueryRow("SELECT coalesce(undo_floor_id,0) FROM sessions WHERE id=?", session).Scan(&floor); err != nil {
		return RestoreTarget{}, err
	}
	floorFound := floor == 0
	pending := map[string]bool{}
	for _, entry := range entries {
		floorFound = floorFound || entry.ID == floor
		if !entry.Visible {
			continue
		}
		if entry.Kind == "tool_result" {
			var ref struct {
				CallID string `json:"call_id"`
			}
			if err = json.Unmarshal(entry.Content, &ref); err != nil {
				return RestoreTarget{}, err
			}
			var providerID string
			if err = s.DB.QueryRow("SELECT provider_call_id FROM tool_calls WHERE id=?", ref.CallID).Scan(&providerID); err != nil {
				return RestoreTarget{}, err
			}
			if !pending[providerID] {
				return RestoreTarget{}, fmt.Errorf("branch #%d has an unpaired tool result", id)
			}
			delete(pending, providerID)
			continue
		}
		if entry.Role != "assistant" {
			continue
		}
		var message provider.Message
		if err = json.Unmarshal(entry.Content, &message); err != nil {
			return RestoreTarget{}, err
		}
		for _, call := range message.Calls {
			if call.ID == "" || pending[call.ID] {
				return RestoreTarget{}, fmt.Errorf("branch #%d has invalid tool calls", id)
			}
			pending[call.ID] = true
		}
	}
	if !floorFound {
		return RestoreTarget{}, errors.New("branch crosses undo boundary; select an entry descended from the current boundary")
	}
	if len(pending) != 0 {
		return RestoreTarget{}, fmt.Errorf("branch #%d has pending main tool results; select an entry after the complete tool batch", id)
	}
	return target, nil
}
