package openai

import (
	"testing"

	"ttc/internal/llm"
)

func TestTokenizerIDKnownFamilies(t *testing.T) {
	a := &Adapter{}
	for _, version := range []string{"6", "6.1", "6.1.2"} {
		family := "gpt-" + version
		for _, suffix := range []string{"sol", "luna", "terra", "astra"} {
			id := family + "-" + suffix
			t.Run(id, func(t *testing.T) {
				if got := a.TokenizerID(llm.ModelSpec{ID: id}); got != "version:"+family {
					t.Fatalf("TokenizerID(%q) = %q, want version:%s", id, got, family)
				}
			})
		}
	}
	if a.TokenizerID(llm.ModelSpec{ID: "gpt-6-sol"}) == a.TokenizerID(llm.ModelSpec{ID: "gpt-6.1-luna"}) {
		t.Fatal("different numeric versions share a tokenizer identity")
	}
}

func TestTokenizerIDUnknownModelsRemainExact(t *testing.T) {
	a := &Adapter{}
	for _, id := range []string{
		"", "gpt-6", "gpt-6.1", "gpt-6-mini", "gpt-6-sol-fast",
		"gpt-6-sol-2026-10-22", "gpt-6-2026-10-22-sol", "gpt-6.1-unknown",
		"gpt-6sol", "gpt-6.-sol", "gpt-.6-sol", "gpt-6..1-sol",
		"gpt-six-sol", "gpt-6.1x-sol", "gpt-6-SOL", "GPT-6-sol",
		"other-gpt-6-sol", "other-6-sol", "o6-sol", "gpt-6-sol\n",
		"version:gpt-6", "model:gpt-6",
	} {
		t.Run(id, func(t *testing.T) {
			want := "model:" + id
			if id == "" {
				want = ""
			}
			if got := a.TokenizerID(llm.ModelSpec{ID: id}); got != want {
				t.Fatalf("TokenizerID(%q) = %q, want %q", id, got, want)
			}
			if id != "" && a.TokenizerID(llm.ModelSpec{ID: id}) == a.TokenizerID(llm.ModelSpec{ID: "gpt-6-sol"}) {
				t.Fatal("unknown model collided with shared family")
			}
		})
	}
}

func TestTokenizerIDUsesUnderlyingRequestModel(t *testing.T) {
	a := &Adapter{}
	for _, tc := range []struct {
		model llm.ModelSpec
		want  string
	}{
		{llm.ModelSpec{ID: "gpt-6-sol-fast", BaseID: "gpt-6-sol", ServiceTier: "priority"}, "version:gpt-6"},
		{llm.ModelSpec{ID: "gpt-6.1-luna-fast", BaseID: "gpt-6.1-luna", ServiceTier: "priority"}, "version:gpt-6.1"},
		{llm.ModelSpec{ID: "gpt-6-sol", BaseID: "unknown-model", ServiceTier: "priority"}, "model:unknown-model"},
	} {
		if got := a.TokenizerID(tc.model); got != tc.want {
			t.Errorf("TokenizerID(%+v) = %q, want %q", tc.model, got, tc.want)
		}
	}
}
