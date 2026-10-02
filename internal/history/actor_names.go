package history

import (
	"context"
	"fmt"
)

// SubagentNames reads immutable display names, including closed children and
// compaction predecessors. Names never restore live contexts or job handles.
func (s *Store) SubagentNames(ctx context.Context, session string) (map[string]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT e.actor_id,json_extract(e.content_json,'$.child.label')
		FROM entries e JOIN sessions h ON h.id=e.session_id
		WHERE h.lineage_id=(SELECT lineage_id FROM sessions WHERE id=?) AND e.kind='status'
		AND json_extract(e.content_json,'$.type')='child_state'
		GROUP BY e.actor_id,json_extract(e.content_json,'$.child.label')`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]string{}
	for rows.Next() {
		var actor, name string
		if err = rows.Scan(&actor, &name); err != nil {
			return nil, err
		}
		if previous, found := names[actor]; found && previous != name {
			return nil, fmt.Errorf("subagent %s has conflicting display names", actor)
		}
		if actor == "" || name == "" {
			return nil, fmt.Errorf("subagent identity and name must be nonempty")
		}
		names[actor] = name
	}
	return names, rows.Err()
}
