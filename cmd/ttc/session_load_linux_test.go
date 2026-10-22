package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/history"
	"ttc/internal/historytest"
	"ttc/internal/llm"
)

func TestCLIStartupAndSlashLoadCopyOnce(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("CLI requires ripgrep")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, work := filepath.Join(t.TempDir(), "data"), t.TempDir()
	script := filepath.Join(t.TempDir(), "responses.json")
	if err := os.WriteFile(script, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	selection := llm.Selection{Provider: "saved-provider", Model: llm.ScriptModel(), Variant: "none"}
	source := history.NewID("session")
	turn, _, err := store.StartSession(source, work, selection, llm.Message{Type: llm.TypeUserInput, Content: "Saved CLI context"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"startup", "command"} {
		t.Run(entry, func(t *testing.T) {
			cmd := exec.Command(executable, "-test.run=^TestOfflineCLIRequiresNeitherCredentialsNorCatalog$")
			cmd.Env = append(os.Environ(), "TTC_TEST_NEUTRAL_OFFLINE_DATA="+data, "TTC_TEST_NEUTRAL_OFFLINE_WORK="+work, "TTC_TEST_NEUTRAL_OFFLINE_SCRIPT="+script)
			input := "/quit\n"
			if entry == "startup" {
				cmd.Env = append(cmd.Env, "TTC_TEST_NEUTRAL_OFFLINE_SESSION="+source)
			} else {
				input = "/load " + source + "\n/quit\n"
			}
			cmd.Stdin = strings.NewReader(input)
			if output, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(output), "TTC · Linux terminal agent") {
				t.Fatalf("CLI load: %v\n%s", err, output)
			}
		})
	}
	store, err = history.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	list, err := store.Sessions(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("source plus one copy per load: got %d sessions", len(list))
	}
	for _, saved := range list {
		entries, err := store.Messages(saved.ID)
		if err != nil || len(entries) != 1 || entries[0].Content != "Saved CLI context" {
			t.Fatal("load changed saved context", saved.ID, entries, err)
		}
		var switches int
		if err := historytest.DB(t, store).QueryRow(`SELECT count(*) FROM entries WHERE session_id=? AND json_extract(content_json,'$.type')='model_switch'`, saved.ID).Scan(&switches); err != nil {
			t.Fatal(err)
		}
		if saved.ID == source {
			if saved.Model.Provider != "saved-provider" || switches != 0 {
				t.Fatal("load changed source model", saved, switches)
			}
		} else if saved.Model.Provider != "script" || switches != 1 {
			t.Fatal("load did not apply current model exactly once", saved, switches)
		}
	}
}
