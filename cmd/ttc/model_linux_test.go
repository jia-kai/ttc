package main

import (
	"reflect"
	"testing"

	"ttc/internal/llm"
)

func TestStartupSelection(t *testing.T) {
	first := llm.ScriptModel()
	first.ID, first.DefaultVariant, first.Variants = "first", "low", []string{"low", "high"}
	second := first
	second.ID, second.DefaultVariant, second.Images, second.Revision = "second", "high", true, "fresh"
	fast := second
	fast.ID, fast.BaseID, fast.ServiceTier = "second/fast", "second", "priority"
	models := []llm.ModelSpec{first, second, fast}
	saved := func(id, variant string) *llm.Selection {
		// Only saved IDs and the variant should survive catalog refresh.
		return &llm.Selection{Provider: "openai", Model: llm.ModelSpec{ID: id}, Variant: variant}
	}
	foreign := saved("second", "high")
	foreign.Provider = "other"
	for _, tt := range []struct {
		name           string
		models         []llm.ModelSpec
		saved          *llm.Selection
		model, variant string
		want           llm.ModelSpec
		wantVariant    string
		notice, fails  bool
	}{
		{name: "first launch", models: models, want: first, wantVariant: "low"},
		{name: "remembered", models: models, saved: saved("second", "low"), want: second, wantVariant: "low"},
		{name: "fast", models: models, saved: saved("second/fast", "high"), want: fast, wantVariant: "high"},
		{name: "missing model", models: models, saved: saved("removed/fast", "high"), want: first, wantVariant: "low", notice: true},
		{name: "missing variant", models: models, saved: saved("second", "removed"), want: second, wantVariant: "high", notice: true},
		{name: "explicit model default", models: models, saved: saved("second", "high"), model: "first", want: first, wantVariant: "low"},
		{name: "explicit model and variant", models: models, model: "first", variant: "high", want: first, wantVariant: "high"},
		{name: "variant override", models: models, saved: saved("second", "low"), variant: "high", want: second, wantVariant: "high"},
		{name: "other provider", models: models, saved: foreign, want: first, wantVariant: "low"},
		{name: "invalid explicit model", models: models, model: "missing", fails: true},
		{name: "invalid explicit variant", models: models, saved: saved("second", "low"), variant: "missing", fails: true},
		{name: "empty catalog", fails: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, notice, err := startupSelection("openai", tt.models, tt.saved, tt.model, tt.variant)
			if (err != nil) != tt.fails {
				t.Fatalf("selection=%+v error=%v", got, err)
			}
			if tt.fails {
				return
			}
			if got.Provider != "openai" || got.Variant != tt.wantVariant || !reflect.DeepEqual(got.Model, tt.want) || (notice != "") != tt.notice {
				t.Fatalf("selection=%+v notice=%q", got, notice)
			}
		})
	}
}
