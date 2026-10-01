package history

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"scicode/internal/provider"
)

func TestModelPreferencesRestartIsolationAndNoSessions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if got, err := s.LastSelection("openai"); err != nil || got != nil {
		t.Fatal(got, err)
	}
	a := provider.Selection{Provider: "openai", Model: provider.ScriptModel(), Variant: "low"}
	a.Model.ID = "a"
	b := a
	b.Model.ID, b.Model.BaseID, b.Model.ServiceTier, b.Variant = "b/fast", "b", "priority", "high"
	script := provider.Selection{Provider: "script", Model: provider.ScriptModel(), Variant: "none"}
	assertSelection := func(want provider.Selection) {
		t.Helper()
		got, err := s.LastSelection(want.Provider)
		if err != nil || got == nil {
			t.Fatal(got, err)
		}
		actual, _ := json.Marshal(got)
		expected, _ := json.Marshal(provider.Selection{Provider: want.Provider, Model: provider.ModelSpec{ID: want.Model.ID}, Variant: want.Variant})
		if !bytes.Equal(actual, expected) {
			t.Fatalf("preference retained catalog metadata: %s; want %s", actual, expected)
		}
	}
	for _, selection := range []provider.Selection{a, b, script} {
		if err := s.SaveSelection(selection); err != nil {
			t.Fatal(err)
		}
	}
	assertSelection(b)
	assertSelection(script)
	var rows int
	if err := s.DB.QueryRow("SELECT count(*) FROM sessions").Scan(&rows); err != nil || rows != 0 {
		t.Fatal("saving a model created sessions", rows, err)
	}
	info, err := os.Stat(filepath.Join(root, "model-choices.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	assertSelection(b)
	assertSelection(script)
	if err := s.SaveSelection(a); err != nil {
		t.Fatal(err)
	}
	assertSelection(a)
	assertSelection(script)
}

func TestModelPreferencesRejectInvalidFilesAndChoices(t *testing.T) {
	for _, raw := range []string{
		`not json`, `null`, `{}`, `{"version":2,"choices":{}}`, `{"version":1,"choices":null}`,
		`{"version":1,"choices":{},"unknown":true}`, `{"version":1,"choices":{}} {}`,
		`{"version":1,"choices":{"openai":{"id":"m","variant":"low","name":"stale catalog"}}}`,
		`{"version":1,"choices":{"openai":{"id":"m"}}}`, `{"version":1,"choices":{"":{"id":"m","variant":"low"}}}`,
		`{"version":1,"choices":{"openai":{"id":"m\n","variant":"low"}}}`,
		"{\"version\":1,\"choices\":{\"openai\":{\"id\":\"\xff\",\"variant\":\"low\"}}}",
	} {
		t.Run(raw, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "data"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			path := filepath.Join(s.Root, "model-choices.json")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.LastSelection("openai"); err == nil {
				t.Fatal("invalid preferences accepted")
			}
			if err := s.SaveSelection(provider.Selection{Provider: "openai", Model: provider.ModelSpec{ID: "m"}, Variant: "low"}); err == nil {
				t.Fatal("invalid preferences silently overwritten")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != raw {
				t.Fatal("invalid file changed", err)
			}
		})
	}
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, selection := range []provider.Selection{
		{}, {Provider: "openai", Model: provider.ModelSpec{ID: "m"}},
		{Provider: " openai", Model: provider.ModelSpec{ID: "m"}, Variant: "low"},
		{Provider: "openai", Model: provider.ModelSpec{ID: string([]byte{0xff})}, Variant: "low"},
	} {
		if err := s.SaveSelection(selection); err == nil {
			t.Fatal("invalid choice accepted", selection)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Root, "model-choices.json")); !os.IsNotExist(err) {
		t.Fatal("invalid choice created file", err)
	}
}

func TestModelPreferencesRejectUnsafeAndOversizedFiles(t *testing.T) {
	for _, mode := range []string{"permission", "directory", "symlink", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "data"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			path := filepath.Join(s.Root, "model-choices.json")
			switch mode {
			case "permission":
				err = os.WriteFile(path, []byte(`{"version":1,"choices":{}}`), 0644)
				if err == nil {
					err = os.Chmod(path, 0644)
				}
			case "directory":
				err = os.Mkdir(path, 0700)
			case "symlink":
				target := filepath.Join(t.TempDir(), "original")
				if err = os.WriteFile(target, []byte(`{"version":1,"choices":{}}`), 0600); err == nil {
					err = os.Symlink(target, path)
				}
			case "oversized":
				err = os.WriteFile(path, bytes.Repeat([]byte{' '}, modelChoicesBytes+1), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.LastSelection("openai"); err == nil {
				t.Fatal("unsafe preferences accepted")
			}
			if err := s.SaveSelection(provider.Selection{Provider: "openai", Model: provider.ModelSpec{ID: "m"}, Variant: "low"}); err == nil {
				t.Fatal("unsafe preferences replaced")
			}
		})
	}
}

func TestModelPreferencesWriteBoundAndConcurrentProviders(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.SaveSelection(provider.Selection{Provider: fmt.Sprintf("provider-%d", i), Model: provider.ModelSpec{ID: "m"}, Variant: "low"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for i := range 8 {
		if got, err := s.LastSelection(fmt.Sprintf("provider-%d", i)); err != nil || got == nil {
			t.Fatal("concurrent save lost provider", got, err)
		}
	}
	choices := modelChoices{Version: 1, Choices: map[string]modelChoice{}}
	var before []byte
	var next provider.Selection
	for i := 0; ; i++ {
		providerID := fmt.Sprintf("provider-%d", i)
		choice := modelChoice{ID: strings.Repeat("m", 512), Variant: strings.Repeat("v", 128)}
		choices.Choices[providerID] = choice
		data, err := json.Marshal(choices)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > modelChoicesBytes {
			next = provider.Selection{Provider: providerID, Model: provider.ModelSpec{ID: choice.ID}, Variant: choice.Variant}
			break
		}
		before = data
	}
	path := filepath.Join(s.Root, "model-choices.json")
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSelection(next); err == nil {
		t.Fatal("oversized preference write accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed write changed preferences", err)
	}
}

func TestSwitchModelDatabaseRollbackDoesNotChangePreference(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	previous := provider.Selection{Provider: "script", Model: provider.ScriptModel(), Variant: "none"}
	id := NewID("session")
	if _, _, err := s.StartSession(id, t.TempDir(), previous, provider.Message{Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LastSelection(previous.Provider); err != nil || got != nil {
		t.Fatal("session history inferred a preference", got, err)
	}
	next := previous
	next.Model.ID = "selected-next"
	if err := s.SaveSelection(next); err != nil {
		t.Fatal(err)
	}
	before, err := s.Branch(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("CREATE TRIGGER reject_switch BEFORE INSERT ON entries BEGIN SELECT RAISE(ABORT,'switch blocked'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SwitchModel(id, "", previous, next, "switch"); err == nil {
		t.Fatal("ignored failed switch")
	}
	saved, err := s.Session(id)
	if err != nil || saved.Model.Model.ID != previous.Model.ID {
		t.Fatal("model update escaped rollback", saved, err)
	}
	after, err := s.Branch(id, 0)
	if err != nil || len(after) != len(before) {
		t.Fatal("switch entry escaped rollback", after, err)
	}
	choice, err := s.LastSelection(previous.Provider)
	if err != nil || choice == nil || choice.Model.ID != next.Model.ID {
		t.Fatal("database failure rewrote explicit preference", choice, err)
	}
	if _, err := s.DB.Exec("DROP TRIGGER reject_switch"); err != nil {
		t.Fatal(err)
	}
	applied := previous
	applied.Model.ID = "another-applied-model"
	if _, err := s.SwitchModel(id, "", previous, applied, "switch"); err != nil {
		t.Fatal(err)
	}
	choice, err = s.LastSelection(previous.Provider)
	if err != nil || choice == nil || choice.Model.ID != next.Model.ID {
		t.Fatal("applied switch rewrote explicit preference", choice, err)
	}
}
