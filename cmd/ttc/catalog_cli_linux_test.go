package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ttc/internal/providers/openai"
)

func awaitCatalogSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("catalog request did not make progress")
	}
}

func TestCatalogCLIColdAndWarmStartup(t *testing.T) {
	if data := os.Getenv("TTC_TEST_CATALOG_CLI_DATA"); data != "" {
		flag.CommandLine = flag.NewFlagSet("ttc", flag.ContinueOnError)
		os.Args = []string{"ttc", "--plain", "--data-dir", data, "--workdir", os.Getenv("TTC_TEST_CATALOG_CLI_WORK"), "--openai-base-url", os.Getenv("TTC_TEST_CATALOG_CLI_ENDPOINT")}
		if err := run(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("CLI requires ripgrep")
	}
	var requests atomic.Int32
	var blockRefresh atomic.Bool
	refreshStarted := make(chan struct{})
	refreshCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/models" {
			t.Errorf("unexpected startup request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", 400)
			return
		}
		requests.Add(1)
		if blockRefresh.Load() {
			close(refreshStarted)
			<-r.Context().Done()
			close(refreshCanceled)
			return
		}
		fmt.Fprint(w, `{"models":[{"slug":"cached-model","display_name":"Cached Model","visibility":"list","context_window":100000,"default_reasoning_level":"low","supported_reasoning_levels":[{"effort":"low"}]}]}`)
	}))
	defer server.Close()
	data := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	importTestCredentials(t, openai.NewAuthenticator(filepath.Join(data, "openai-auth.json")), "test")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	command := func() *exec.Cmd {
		cmd := exec.Command(executable, "-test.run=^TestCatalogCLIColdAndWarmStartup$")
		cmd.Env = append(os.Environ(), "TTC_TEST_CATALOG_CLI_DATA="+data, "TTC_TEST_CATALOG_CLI_WORK="+work, "TTC_TEST_CATALOG_CLI_ENDPOINT="+server.URL)
		return cmd
	}
	cold := command()
	cold.Stdin = strings.NewReader("/quit\n")
	if output, err := cold.CombinedOutput(); err != nil || !bytes.Contains(output, []byte("TTC · Linux terminal agent")) {
		t.Fatalf("cold CLI: %v\n%s", err, output)
	}
	if requests.Load() != 1 {
		t.Fatal("cold request count", requests.Load())
	}
	cacheFile := filepath.Join(data, "openai-models.json")
	cacheRaw, err := os.ReadFile(cacheFile)
	if err != nil {
		t.Fatal("cold startup did not persist catalog", err)
	}
	var envelope struct {
		Version        int               `json:"version"`
		Provider       string            `json:"provider"`
		CatalogVersion string            `json:"catalog_version"`
		Models         []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(cacheRaw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Version != 2 || envelope.Provider != "openai" || envelope.CatalogVersion != openai.CatalogVersion || len(envelope.Models) != 1 {
		t.Fatalf("unexpected neutral catalog envelope: %s", cacheRaw)
	}
	if strings.Contains(string(cacheRaw), server.URL) || strings.Contains(string(cacheRaw), `"budget"`) || strings.Contains(string(cacheRaw), `"access_token"`) {
		t.Fatalf("cache contains endpoint, policy or credentials: %s", cacheRaw)
	}
	if !strings.Contains(string(envelope.Models[0]), `"limits"`) {
		t.Fatalf("cache contains no pure model limits: %s", cacheRaw)
	}

	blockRefresh.Store(true)
	warm := command()
	stdin, err := warm.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := warm.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	warm.Stderr = &stderr
	if err := warm.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	finished := make(chan error, 1)
	go func() { finished <- warm.Wait() }()
	waited := false
	defer func() {
		stdin.Close()
		if !waited {
			warm.Process.Kill()
			<-finished
		}
	}()
	waitLine := func(needle string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case line, open := <-lines:
				if !open {
					t.Fatalf("warm CLI exited before %q", needle)
				}
				if strings.Contains(line, needle) {
					return
				}
			case <-deadline:
				t.Fatalf("warm CLI blocked before %q", needle)
			}
		}
	}
	waitLine("TTC · Linux terminal agent")
	awaitCatalogSignal(t, refreshStarted)
	fmt.Fprintln(stdin, "/model")
	waitLine("cached-model · Cached Model")
	fmt.Fprintln(stdin, "/quit")
	select {
	case err := <-finished:
		waited = true
		if err != nil {
			t.Fatalf("warm CLI exit: %v\n%s", err, stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("warm CLI failed to cancel and join refresh")
	}
	awaitCatalogSignal(t, refreshCanceled)
	if requests.Load() != 2 {
		t.Fatal("unexpected request count", requests.Load())
	}

	// A structurally valid cache with an invalid choice must be treated as
	// unusable and synchronously replaced, rather than failing SaveSelection.
	blockRefresh.Store(false)
	cachePath := filepath.Join(data, "openai-models.json")
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	var cache map[string]json.RawMessage
	if err := json.Unmarshal(raw, &cache); err != nil {
		t.Fatal(err)
	}
	var models []map[string]json.RawMessage
	if err := json.Unmarshal(cache["models"], &models); err != nil {
		t.Fatal(err)
	}
	models[0]["variants"] = json.RawMessage(`["low "]`)
	models[0]["default_variant"] = json.RawMessage(`"low "`)
	cache["models"], err = json.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	invalid := command()
	invalid.Stdin = strings.NewReader("/quit\n")
	output, err := invalid.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("Ignoring invalid model catalog cache")) || !bytes.Contains(output, []byte("TTC · Linux terminal agent")) {
		t.Fatalf("invalid cache was not refetched: %v\n%s", err, output)
	}
	if requests.Load() != 3 {
		t.Fatal("invalid cache did not trigger synchronous fetch", requests.Load())
	}
}
