package history

import (
	"context"
	"encoding/json"
	"fmt"

	"ttc/internal/llm"
	"ttc/internal/messagestream"
)

// Replay is a selected conversation's presentation, ordered for display. Name is
// the session name at acquisition; Rows contain no storage ancestry or envelopes.
type Replay struct {
	Name string
	Rows []ReplayRow
}

// ReplayRow presents either a grouped stream or one saved conversation item.
// ID is the inspection/live-update identity. Stream, when nonnil, is the entire
// row. Message is canonical typed content, with original input admission time.
// Label is empty for Message-bearing rows; the frontend formats their content.
// ToolCallID pairs pending tools with live updates; ToolComplete marks results.
// Execution offers execution inspection, including job completion notifications.
type ReplayRow struct {
	ID              int64
	Actor, Label    string
	System          bool
	Execution       bool
	ToolCallID      string
	ToolComplete    bool
	Message         *llm.Message
	ChildCompletion *ChildCompletionPresentation
	Stream          *messagestream.Item
}

// ReadReplay acquires presentation rows through one immutable selected cut.
// A zero tip snapshots the current session tip exactly once (including an empty
// session); an explicit tip excludes later tool results and stream revisions.
// Tool details, instruction files, and native request payloads are not loaded.
func (s *Store) ReadReplay(ctx context.Context, session string, explicitTip int64) (replay Replay, err error) {
	defer func() { err = intentError(fmt.Sprintf("read session %s replay", session), err) }()
	q := contextHistoryReader{ctx, s.db}
	var current int64
	if err := q.QueryRow("SELECT name,coalesce(active_entry_id,0) FROM sessions WHERE id=?", session).Scan(&replay.Name, &current); err != nil {
		return Replay{}, err
	}
	tip := explicitTip
	if tip == 0 {
		tip = current
	}
	if err := validateHistoryTip(q, session, tip); err != nil {
		return Replay{}, err
	}
	if tip == 0 {
		return replay, ctx.Err()
	}
	entries, err := branchWith(q, session, tip, false)
	if err != nil {
		return Replay{}, err
	}
	callIDs, results, err := replayToolResults(entries)
	if err != nil {
		return Replay{}, err
	}
	stream, err := s.StreamItems(ctx, tip)
	if err != nil {
		return Replay{}, err
	}
	for i, item := range stream {
		if item.ID <= 0 || i > 0 && item.ID <= stream[i-1].ID {
			return Replay{}, fmt.Errorf("stream items must have increasing saved entry IDs")
		}
	}
	nextStream := 0
	appendStream := func() {
		item := stream[nextStream]
		replay.Rows = append(replay.Rows, ReplayRow{ID: item.ID, Actor: item.Actor, Stream: &item})
		nextStream++
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return Replay{}, err
		}
		for nextStream < len(stream) && stream[nextStream].ID < entry.ID {
			appendStream()
		}
		if nextStream < len(stream) && stream[nextStream].ID == entry.ID {
			appendStream()
			continue
		}
		if entry.InternalEvent() {
			continue
		}
		callID := callIDs[entry.ID]
		if entry.Kind == "tool_call" && results[callID] != 0 || entry.Kind == "tool_result" && results[callID] != entry.ID {
			continue
		}
		data, completion, err := loadViewPresentation(ctx, q, entry)
		if err != nil {
			return Replay{}, err
		}
		if data.Status.Type == "system_prompt" || data.Status.Type == "request_message" || data.Status.Type == "tool_stream" {
			continue
		}
		row := ReplayRow{
			ID: entry.ID, Actor: entry.Actor,
			System:     entry.Kind == "status" || entry.Role == "system",
			Execution:  entry.Kind == "tool_call" || entry.Kind == "tool_result" || data.Status.Type == "job_completion",
			ToolCallID: callID, ToolComplete: entry.Kind == "tool_result",
		}
		if (entry.Kind == "message" || entry.Kind == "summary") && data.Message != nil {
			message := *data.Message
			switch message.Type {
			case llm.TypeRuntimeContext, llm.TypeCompactionContext, llm.TypeSystemInstruction, llm.TypeDeveloperInstruction:
				continue // Model-input snapshots are presented at request boundaries.
			}
			if entry.Kind == "message" && message.Type == llm.TypeUserInput {
				message.InputTimeMS = entry.CreatedMS
			}
			row.Message, row.ChildCompletion = &message, completion
		} else if entry.Kind == "tool_call" {
			row.Label, err = s.exportTextWith(ctx, q, entry)
			if err != nil {
				return Replay{}, err
			}
		} else {
			row.Label = projectLabel(data)
		}
		replay.Rows = append(replay.Rows, row)
	}
	for nextStream < len(stream) {
		appendStream()
	}
	return replay, ctx.Err()
}

// Only selected results supersede an intent; the latest stored tool result may
// belong to a later cut or an abandoned branch.
func replayToolResults(entries []Entry) (map[int64]string, map[string]int64, error) {
	results := make(map[string]int64)
	callIDs := make(map[int64]string)
	for _, entry := range entries {
		if entry.Kind != "tool_call" && entry.Kind != "tool_result" {
			continue
		}
		var ref struct {
			CallID string `json:"call_id"`
		}
		if err := json.Unmarshal(entry.Content, &ref); err != nil || ref.CallID == "" {
			return nil, nil, fmt.Errorf("invalid tool reference in entry %d", entry.ID)
		}
		callIDs[entry.ID] = ref.CallID
		if entry.Kind == "tool_result" {
			results[ref.CallID] = entry.ID
		}
	}
	return callIDs, results, nil
}
