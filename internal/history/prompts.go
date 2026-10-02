package history

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// MaxPromptHistoryEntries bounds prompt recall and search across saved sessions.
const MaxPromptHistoryEntries = 1000

// MaxPromptHistoryBytes bounds retained authored prompt text in UTF-8 bytes.
const MaxPromptHistoryBytes = 8 << 20

// PromptHistory returns recent authored main-agent prompts from all sessions,
// oldest first. It excludes runtime messages, attachment snapshots and copied
// compaction entries. The newest 1,000 entries are read with an 8 MiB text budget;
// prompts are never truncated. Commands are not persisted conversation messages.
func (s *Store) PromptHistory(ctx context.Context) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT substr(coalesce(json_extract(e.content_json,'$.user_text'),
		json_extract(e.content_json,'$.content'),''),1,?)
		FROM entries e JOIN turns t ON t.id=e.turn_id
		WHERE e.kind='message' AND e.role='user' AND e.actor_id='main'
		AND e.model_visible=1 AND e.source_id IS NULL AND t.trigger IN ('user','steer')
		AND coalesce(json_extract(e.content_json,'$.runtime'),0)=0
		ORDER BY e.id DESC LIMIT ?`, MaxPromptHistoryBytes+1, MaxPromptHistoryEntries)
	if err != nil {
		return nil, fmt.Errorf("read prompt history: %w", err)
	}
	defer rows.Close()
	var prompts []string
	bytes := 0
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("decode prompt history: %w", err)
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if len(text) > MaxPromptHistoryBytes {
			continue
		}
		if len(text) > MaxPromptHistoryBytes-bytes {
			break
		}
		prompts = append(prompts, text)
		bytes += len(text)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read prompt history: %w", err)
	}
	slices.Reverse(prompts)
	return prompts, nil
}
