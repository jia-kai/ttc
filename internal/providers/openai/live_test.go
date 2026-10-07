package openai_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
	"ttc/internal/binaryinput"
	"ttc/internal/catalog"
	"ttc/internal/llm"
	"ttc/internal/providers/openai"
)

// TestLiveSmoke is opt-in and makes exactly one tiny inference request. All other tests are local.
func TestLiveSmoke(t *testing.T) {
	path := os.Getenv("TTC_LIVE_AUTH")
	if path == "" {
		t.Skip("set TTC_LIVE_AUTH to a private imported credential file for a single smoke request")
	}
	a := openai.NewAdapter(openai.Config{TokenSource: openai.NewAuthenticator(path).AccessTokens, ResolveBinary: binaryinput.Resolve})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	manager, e := catalog.Open(ctx, catalog.Config{Bind: func(context.Context) (catalog.Binding, error) {
		return catalog.Binding{Scope: catalog.Scope{Provider: "openai", Endpoint: a.BaseURL, Version: openai.CatalogVersion}, Source: a, Validate: openai.ValidateCatalog}, nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	defer manager.Close()
	model := os.Getenv("TTC_LIVE_MODEL")
	selection, e := llm.Resolve("openai", manager.Initial, model, "low")
	if e != nil {
		t.Fatal(e)
	}
	var text strings.Builder
	var usage *llm.Usage
	e = a.Stream(ctx, llm.Request{ConversationID: "test-conversation", Selection: selection, System: "Reply with exactly TTC_OK. Use no reasoning or tools. This is an authentication smoke test.", Messages: []llm.Message{{Role: "user", Content: "Return TTC_OK"}}, NoTools: true, OutputTokens: 16}, func(event llm.StreamEvent) error {
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
