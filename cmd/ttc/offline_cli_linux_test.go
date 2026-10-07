package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/providers/openai"
)

func importTestCredentials(t *testing.T, owner *openai.Authenticator, account string) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "codex.json")
	raw, err := json.Marshal(openai.Credentials{AuthMode: "chatgpt", Tokens: openai.CredentialsTokens{Access: "synthetic", AccountID: account}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := owner.ImportCodex(context.Background(), source); err != nil {
		t.Fatal(err)
	}
}

func TestOfflineCLIRequiresNeitherCredentialsNorCatalog(t *testing.T) {
	if data := os.Getenv("TTC_TEST_NEUTRAL_OFFLINE_DATA"); data != "" {
		flag.CommandLine = flag.NewFlagSet("ttc", flag.ContinueOnError)
		os.Args = []string{"ttc", "--plain", "--data-dir", data, "--workdir", os.Getenv("TTC_TEST_NEUTRAL_OFFLINE_WORK"), "--offline-script", os.Getenv("TTC_TEST_NEUTRAL_OFFLINE_SCRIPT")}
		if err := run(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("CLI requires ripgrep")
	}
	data := filepath.Join(t.TempDir(), "data")
	script := filepath.Join(t.TempDir(), "responses.json")
	if err := os.WriteFile(script, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestOfflineCLIRequiresNeitherCredentialsNorCatalog$")
	cmd.Env = append(os.Environ(), "TTC_TEST_NEUTRAL_OFFLINE_DATA="+data, "TTC_TEST_NEUTRAL_OFFLINE_WORK="+t.TempDir(), "TTC_TEST_NEUTRAL_OFFLINE_SCRIPT="+script)
	cmd.Stdin = strings.NewReader("/quit\n")
	if output, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(output), "TTC · Linux terminal agent") {
		t.Fatalf("offline CLI failed: %v\n%s", err, output)
	}
	for _, name := range []string{"openai-auth.json", "openai-auth.json.lock", "openai-models.json"} {
		if _, err := os.Stat(filepath.Join(data, name)); !os.IsNotExist(err) {
			t.Fatalf("offline startup touched %s: %v", name, err)
		}
	}
}
