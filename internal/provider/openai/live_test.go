package openai

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
	"ttc/internal/provider"
)

// TestLiveSmoke is opt-in and makes exactly one tiny inference request. All other tests are local.
func TestLiveSmoke(t *testing.T) {
	path := os.Getenv("TTC_LIVE_AUTH")
	if path == "" {
		t.Skip("set TTC_LIVE_AUTH to a private imported credential file for a single smoke request")
	}
	a := New(path)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	models, e := a.Models(ctx)
	if e != nil {
		t.Fatal(e)
	}
	model := os.Getenv("TTC_LIVE_MODEL")
	selection, e := provider.Resolve("openai", models, model, "low")
	if e != nil {
		t.Fatal(e)
	}
	var text strings.Builder
	var usage *provider.Usage
	e = a.Stream(ctx, provider.Request{ConversationID: "test-conversation", Selection: selection, System: "Reply with exactly TTC_OK. Use no reasoning or tools. This is an authentication smoke test.", Messages: []provider.Message{{Role: "user", Content: "Return TTC_OK"}}, NoTools: true, OutputTokens: 16}, func(event provider.StreamEvent) error {
		if event.Kind == "text" {
			text.WriteString(event.Text)
		}
		if event.Kind == "completed" {
			usage = event.Usage
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if strings.TrimSpace(text.String()) != "TTC_OK" {
		t.Fatalf("unexpected smoke response: %q", text.String())
	}
	t.Logf("single smoke succeeded; reported usage: %+v", usage)
}
