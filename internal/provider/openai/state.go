package openai

import (
	"encoding/json"
	"errors"
	"strings"

	"ttc/internal/provider"
)

const replayVersion = 1

// replayItems validates the native output against canonical history before
// replaying it. Assistant phases, reasoning and original item IDs stay intact.
func replayItems(m provider.Message) ([]any, error) {
	if m.State.Version != replayVersion || m.Role != "assistant" || len(m.State.Items) == 0 {
		return nil, errors.New("unsupported OpenAI replay state")
	}
	var content strings.Builder
	var calls []provider.ToolCall
	var out []any
	for _, raw := range m.State.Items {
		var item struct {
			Type      string `json:"type"`
			Role      string `json:"role"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		switch item.Type {
		case "reasoning":
		case "message":
			if item.Role != "assistant" {
				return nil, errors.New("non-assistant OpenAI replay message")
			}
			for _, part := range item.Content {
				switch part.Type {
				case "output_text":
					content.WriteString(part.Text)
				case "refusal":
					content.WriteString(part.Refusal)
				default:
					return nil, errors.New("unsupported OpenAI replay content")
				}
			}
		case "function_call":
			calls = append(calls, provider.ToolCall{ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(item.Arguments)})
		default:
			return nil, errors.New("unsupported OpenAI replay item")
		}
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if content.String() != m.Content || len(calls) != len(m.Calls) {
		return nil, errors.New("OpenAI replay state disagrees with canonical response")
	}
	for i, call := range calls {
		original := m.Calls[i]
		// History marshals RawMessage arguments, compacting whitespace and
		// escaping HTML. Native arguments are strings and retain their original
		// formatting. Normalize both with the same marshaler, preserving numbers.
		args, err := json.Marshal(call.Arguments)
		if err != nil {
			return nil, err
		}
		canonical, err := json.Marshal(original.Arguments)
		if err != nil {
			return nil, err
		}
		if call.ID != original.ID || call.Name != original.Name || string(args) != string(canonical) {
			return nil, errors.New("OpenAI replay tool disagrees with canonical call")
		}
	}
	return out, nil
}

type wireUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	InputDetails struct {
		Cached  *int `json:"cached_tokens"`
		Written *int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		Reasoning *int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u *wireUsage) normalized() *provider.Usage {
	if u == nil {
		return nil
	}
	return &provider.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedInputTokens: u.InputDetails.Cached, CacheWriteTokens: u.InputDetails.Written, ReasoningOutputTokens: u.OutputDetails.Reasoning}
}
