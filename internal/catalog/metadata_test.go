package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestSecondProviderRejectsContradictoryCapacity(t *testing.T) {
	models := model("contradictory")
	models[0].Limits.MaxOutputTokens = models[0].Limits.ContextLimit + 1
	c := config("", sourceFunc(func(context.Context) ([]llm.ModelInfo, error) { return models, nil }))
	m, err := Open(context.Background(), c)
	if m != nil {
		m.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "invalid capacities") {
		t.Fatalf("contradictory second-provider capacities accepted: %v", err)
	}

	// Disk-loaded metadata must pass the same generic validation without any
	// provider-specific callback, rather than being trusted as prepared output.
	b := binding(nil)
	path := filepath.Join(privateTempDir(t), "models.json")
	putRecord(t, path, record(b.Scope, models))
	if _, err := readCache(context.Background(), path, b); !errors.Is(err, ErrInvalidCache) || !strings.Contains(err.Error(), "invalid capacities") {
		t.Fatalf("contradictory cached capacities accepted: %v", err)
	}
}

func TestMetadataValidationRunsGenericBeforeProviderExtras(t *testing.T) {
	called := false
	failure := errors.New("provider constraint")
	extra := func([]llm.ModelInfo) error {
		called = true
		return failure
	}
	models := model("valid")
	models[0].Limits.MaxOutputTokens = models[0].Limits.ContextLimit + 1
	if err := validate(models, extra); err == nil || called {
		t.Fatalf("generic validation did not stop invalid metadata: called=%v, err=%v", called, err)
	}
	models[0].Limits.MaxOutputTokens = 0
	if err := validate(models, extra); !errors.Is(err, failure) || !called {
		t.Fatalf("provider validation not applied: called=%v, err=%v", called, err)
	}
}
