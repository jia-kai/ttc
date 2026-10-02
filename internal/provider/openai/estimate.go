package openai

import (
	"encoding/json"

	"scicode/internal/provider"
)

// EstimateReplay counts model-visible content rather than serialized envelopes.
// Encrypted reasoning uses Codex's coarse decoded-size heuristic: remove base64
// expansion and 650 bytes of encryption overhead, then divide by four bytes per
// token. This is an estimate, not tokenization or official usage. Unknown codecs
// stay conservatively bounded by their transport size until wire validation.
func (a *Adapter) EstimateReplay(m provider.Message) int {
	if m.State == nil {
		return 0
	}
	if m.State.Provider != "openai" || m.State.Version != replayVersion {
		return provider.ReplayTokens(m.State)
	}
	tokens := 0
	for _, raw := range m.State.Items {
		var item struct {
			Type, Name, Arguments string
			Encrypted             string `json:"encrypted_content"`
			Content               []struct{ Text, Refusal string }
			Summary               []struct{ Text string }
		}
		if json.Unmarshal(raw, &item) != nil {
			return provider.ReplayTokens(m.State)
		}
		switch item.Type {
		case "reasoning":
			if item.Encrypted != "" {
				decoded := len(item.Encrypted)/4*3 + len(item.Encrypted)%4*3/4
				tokens += (max(0, decoded-650) + 3) / 4
			} else {
				for _, part := range item.Summary {
					tokens += (len(part.Text) + 2) / 3
				}
			}
		case "message":
			tokens += 16
			for _, part := range item.Content {
				tokens += (len(part.Text) + len(part.Refusal) + 2) / 3
			}
		case "function_call":
			tokens += 16 + (len(item.Name)+2)/3 + (len(item.Arguments)+2)/3
		default:
			return provider.ReplayTokens(m.State)
		}
	}
	return tokens
}
