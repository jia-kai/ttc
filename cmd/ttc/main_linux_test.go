package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"scicode/internal/history"
	"scicode/internal/provider"
	"scicode/internal/provider/openai"
)

type catalogTransport func(*http.Request) (*http.Response, error)

func (f catalogTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestStartupWithoutModelAndSwitchAcrossRestarts(t *testing.T) {
	if data := os.Getenv("TTC_TEST_MODEL_DATA"); data != "" {
		// Run the real CLI with an in-process HTTP transport: no sockets or auth.
		catalogs := 0
		http.DefaultTransport = catalogTransport(func(req *http.Request) (*http.Response, error) {
			if req.Method != "GET" || req.URL.Path != "/backend-api/codex/models" {
				t.Errorf("startup/model switch made an inference or auth request: %s %s", req.Method, req.URL)
				return nil, fmt.Errorf("unexpected HTTP request")
			}
			catalogs++
			if os.Getenv("TTC_TEST_MODEL_CATALOG_RETRY") == "1" && catalogs <= 2 {
				return &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": {"0"}}, Body: io.NopCloser(strings.NewReader("unavailable")), Request: req}, nil
			}
			body := `{"models":[{"slug":"first","display_name":"First","visibility":"list","priority":1,"context_window":100000,"default_reasoning_level":"low","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}]},{"slug":"second","display_name":"Second","visibility":"list","priority":2,"context_window":100000,"default_reasoning_level":"low","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}],"service_tiers":[{"id":"priority","name":"Fast"}]}]}`
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})
		flag.CommandLine = flag.NewFlagSet("ttc", flag.ContinueOnError)
		os.Args = []string{"ttc", "--data-dir", data, "--workdir", os.Getenv("TTC_TEST_MODEL_WORK")}
		if os.Getenv("TTC_TEST_MODEL_TUI") != "1" {
			os.Args = append(os.Args, "--plain")
		}
		if load := os.Getenv("TTC_TEST_MODEL_LOAD"); load != "" {
			os.Args = append(os.Args, "--session", load, "--model", "second/fast", "--variant", "high")
		}
		err := run()
		if want := os.Getenv("TTC_TEST_MODEL_ERROR"); want != "" {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("startup error=%v; want %q", err, want)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		wantCatalogs := 1
		if os.Getenv("TTC_TEST_MODEL_CATALOG_RETRY") == "1" {
			wantCatalogs = 3
		}
		if catalogs != wantCatalogs {
			t.Fatalf("catalog requests: %d", catalogs)
		}
		return
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("CLI requires ripgrep")
	}
	data, work := filepath.Join(t.TempDir(), "data"), t.TempDir()
	s, err := history.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	credentials, err := json.Marshal(openai.Credentials{AuthMode: "chatgpt", Tokens: openai.Tokens{Access: "synthetic", AccountID: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(data, "openai-auth.json"), credentials, 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		input, model, variant string
		retryCatalog          bool
	}{
		{"/quit\n", "first", "low", false},
		{"/model second/fast high\n/quit\n", "second/fast", "high", false},
		{"/quit\n", "second/fast", "high", true},
	} {
		cmd := exec.Command(executable, "-test.run=^TestStartupWithoutModelAndSwitchAcrossRestarts$")
		cmd.Env = append(os.Environ(), "TTC_TEST_MODEL_DATA="+data, "TTC_TEST_MODEL_WORK="+work)
		if step.retryCatalog {
			cmd.Env = append(cmd.Env, "TTC_TEST_MODEL_CATALOG_RETRY=1")
		}
		cmd.Stdin = strings.NewReader(step.input)
		output, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "TTC · Linux terminal agent") {
			t.Fatalf("CLI did not open without --model: %v\n%s", err, output)
		}
		if strings.HasPrefix(step.input, "/model ") && !strings.Contains(string(output), "Model switched · second/fast · high") {
			t.Fatalf("runtime switch missing: %s", output)
		}
		s, err := history.Open(data)
		if err != nil {
			t.Fatal(err)
		}
		got, readErr := s.LastSelection("openai")
		for _, table := range []string{"workspaces", "sessions", "turns", "entries"} {
			var count int
			if err := s.DB.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
				s.Close()
				t.Fatalf("empty CLI startup/model choice wrote %s: %d, %v", table, count, err)
			}
		}
		closeErr := s.Close()
		if readErr != nil || closeErr != nil || got == nil || got.Model.ID != step.model || got.Variant != step.variant {
			t.Fatalf("remembered=%+v read=%v close=%v", got, readErr, closeErr)
		}
	}
}

func TestStartupLoadModelFailureOrdering(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("CLI requires ripgrep")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"preference", "switch"} {
		t.Run(failure, func(t *testing.T) {
			data, work := filepath.Join(t.TempDir(), "data"), t.TempDir()
			s, err := history.Open(data)
			if err != nil {
				t.Fatal(err)
			}
			previous := provider.Selection{Provider: "openai", Model: provider.ScriptModel(), Variant: "low"}
			previous.Model.ID = "first"
			id := history.NewID("session")
			turn, _, err := s.StartSession(id, work, previous, provider.Message{Role: "user", Content: "Saved research"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.FinishTurn(turn, "completed"); err != nil {
				t.Fatal(err)
			}
			before, err := s.Branch(id, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.SaveSelection(previous); err != nil {
				t.Fatal(err)
			}
			wantError := "startup switch blocked"
			if failure == "preference" {
				wantError = "decode model preferences"
				if err := os.WriteFile(filepath.Join(data, "model-choices.json"), []byte("invalid JSON"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if _, err := s.DB.Exec("CREATE TRIGGER reject_startup_switch BEFORE UPDATE OF model_json ON sessions BEGIN SELECT RAISE(ABORT,'startup switch blocked'); END"); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			credentials, err := json.Marshal(openai.Credentials{AuthMode: "chatgpt", Tokens: openai.Tokens{Access: "synthetic", AccountID: "test"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(data, "openai-auth.json"), credentials, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(executable, "-test.run=^TestStartupWithoutModelAndSwitchAcrossRestarts$")
			cmd.Env = append(os.Environ(), "TTC_TEST_MODEL_DATA="+data, "TTC_TEST_MODEL_WORK="+work, "TTC_TEST_MODEL_LOAD="+id, "TTC_TEST_MODEL_ERROR="+wantError)
			cmd.Stdin = strings.NewReader("/quit\n")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("CLI failure regression failed: %v\n%s", err, output)
			}
			s, err = history.Open(data)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			saved, err := s.Session(id)
			if err != nil {
				t.Fatal(err)
			}
			gotModel, _ := json.Marshal(saved.Model)
			oldModel, _ := json.Marshal(previous)
			if string(gotModel) != string(oldModel) {
				t.Fatal("failed startup changed the loaded model", saved.Model)
			}
			after, err := s.Branch(id, 0)
			if err != nil || len(after) != len(before) || after[len(after)-1].ID != before[len(before)-1].ID {
				t.Fatal("failed startup appended history", after, err)
			}
			if failure == "preference" {
				raw, err := os.ReadFile(filepath.Join(data, "model-choices.json"))
				if err != nil || string(raw) != "invalid JSON" {
					t.Fatal("preference failure replaced the file", string(raw), err)
				}
			} else {
				choice, err := s.LastSelection("openai")
				if err != nil || choice == nil || choice.Model.ID != "second/fast" || choice.Variant != "high" {
					t.Fatal("SQL failure lost the accepted startup choice", choice, err)
				}
			}
		})
	}
}

func TestStartupRequiresRipgrepBeforeCreatingState(t *testing.T) {
	if data := os.Getenv("TTC_TEST_NO_RG_DATA"); data != "" {
		flag.CommandLine = flag.NewFlagSet("ttc", flag.ContinueOnError)
		os.Args = []string{"ttc", "--data-dir", data}
		if err := run(); err == nil || !strings.Contains(err.Error(), "ripgrep (rg) is required") {
			t.Fatalf("startup did not explain its missing search dependency: %v", err)
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	data := filepath.Join(empty, "data")
	cmd := exec.Command(executable, "-test.run=^TestStartupRequiresRipgrepBeforeCreatingState$")
	cmd.Env = append(os.Environ(), "PATH="+empty, "TTC_TEST_NO_RG_DATA="+data)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("startup test failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatalf("missing rg must fail before creating persistent state: %v", err)
	}
}
