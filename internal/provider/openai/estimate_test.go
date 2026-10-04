package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"ttc/internal/provider"
)

func TestReplayEstimateExcludesTransportAndBase64Expansion(t *testing.T) {
	item, err := json.Marshal(map[string]any{"type": "reasoning", "id": "transport", "encrypted_content": strings.Repeat("A", 100000), "summary": []any{map[string]any{"text": strings.Repeat("duplicate summary ", 500)}}})
	if err != nil {
		t.Fatal(err)
	}
	m := provider.Message{Role: "assistant", State: &provider.ReplayState{Provider: "openai", Version: replayVersion, Items: []json.RawMessage{item, json.RawMessage(`{"type":"message","id":"transport2","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`), json.RawMessage(`{"type":"function_call","call_id":"c","name":"read","arguments":"{}"}`)}}}
	a := &Adapter{}
	if got := a.EstimateReplay(m); got != 18588+18+19 {
		t.Fatal("transport bytes were treated as model tokens", got)
	}
	if len(m.State.Items[0]) != len(item) || !strings.Contains(string(m.State.Items[0]), strings.Repeat("A", 100000)) {
		t.Fatal("estimation changed replay content")
	}
	m.State.Items = []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"tiny"}`)}
	if got := a.EstimateReplay(m); got != 0 {
		t.Fatal("fixed encryption overhead counted as reasoning", got)
	}
	m.State.Items = []json.RawMessage{json.RawMessage(`{"type":"reasoning","summary":[{"text":"summary only"}]}`)}
	if got := a.EstimateReplay(m); got != 4 {
		t.Fatal("unencrypted summary lost", got)
	}
}

func TestUnknownReplayEstimateRemainsConservative(t *testing.T) {
	a := &Adapter{}
	for _, item := range []json.RawMessage{json.RawMessage(`{"type":"unknown","data":"payload"}`), json.RawMessage(`not JSON`)} {
		m := provider.Message{State: &provider.ReplayState{Provider: "openai", Version: replayVersion, Items: []json.RawMessage{item}}}
		if got := a.EstimateReplay(m); got != (len(item)+2)/3 {
			t.Fatal("unknown codec was undercounted", got)
		}
	}
}
