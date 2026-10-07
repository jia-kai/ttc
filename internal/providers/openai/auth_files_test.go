package openai

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCredentialFilesRejectUnsafeOrOversizedInput(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "sparse_oversize", "public_mode"} {
		for _, operation := range []string{"load", "import"} {
			if kind == "public_mode" && operation == "import" {
				continue // An explicitly imported source need not have TTC's mode.
			}
			t.Run(operation+"/"+kind, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "synthetic-auth.json")
				var err error
				switch kind {
				case "symlink":
					target := filepath.Join(t.TempDir(), "outside")
					err = os.WriteFile(target, []byte(`{}`), 0600)
					if err == nil {
						err = os.Symlink(target, path)
					}
				case "fifo":
					err = syscall.Mkfifo(path, 0600)
				case "sparse_oversize":
					var f *os.File
					f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
					if err == nil {
						err = f.Truncate(1 << 30)
						f.Close()
					}
				case "public_mode":
					err = os.WriteFile(path, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"synthetic","account_id":"synthetic"}}`), 0644)
					if err == nil {
						err = os.Chmod(path, 0644)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				a := NewAuthenticator(path)
				if operation == "import" {
					a.CredentialPath = filepath.Join(t.TempDir(), "destination", "auth.json")
				}
				done := make(chan error, 1)
				go func() {
					if operation == "import" {
						done <- a.ImportCodex(context.Background(), path)
					} else {
						done <- a.load()
					}
				}()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("unsafe input accepted or not rejected at size boundary", err)
					}
					if kind == "symlink" && !errors.Is(err, syscall.ELOOP) || kind == "fifo" && !strings.Contains(err.Error(), "regular file") || kind == "sparse_oversize" && !strings.Contains(err.Error(), "1 MiB") || kind == "public_mode" && !strings.Contains(err.Error(), "0600") {
						t.Fatal("file boundary failure was hidden by later validation", err)
					}
				case <-time.After(time.Second):
					// Unblock a regression to a blocking FIFO open before failing.
					if f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
						f.Close()
					}
					t.Fatal("credential read blocked")
				}
				if a.credentials != nil {
					t.Fatal("failed read installed credentials")
				}
				if operation == "import" {
					if _, err := os.Stat(a.CredentialPath); !os.IsNotExist(err) {
						t.Fatal("failed import created destination", err)
					}
				}
			})
		}
	}
}

func TestSyntheticCredentialImportAndReload(t *testing.T) {
	source := filepath.Join(t.TempDir(), "synthetic-source.json")
	c := Credentials{AuthMode: "chatgpt", Tokens: CredentialsTokens{ID: "synthetic-id", Access: "synthetic-token", Refresh: "synthetic-refresh", AccountID: "synthetic-account"}}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var fields struct {
		Tokens map[string]string `json:"tokens"`
	}
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields.Tokens) != 4 || fields.Tokens["id_token"] != c.Tokens.ID || fields.Tokens["access_token"] != c.Tokens.Access || fields.Tokens["refresh_token"] != c.Tokens.Refresh || fields.Tokens["account_id"] != c.Tokens.AccountID {
		t.Fatal("auth token JSON fields changed")
	}
	if err := os.WriteFile(source, b, 0644); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "private", "auth.json")
	if err := NewAuthenticator(destination).ImportCodex(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	a := NewAuthenticator(destination)
	if err := a.load(); err != nil || a.credentials == nil || *a.credentials != c {
		t.Fatal("valid synthetic credentials did not round trip", err)
	}
	oversized := c
	oversized.Tokens.Access = strings.Repeat("x", maxCredentialBytes)
	if err := a.save(oversized); err == nil {
		t.Fatal("saved credentials too large to reload")
	}
	if *a.credentials != c {
		t.Fatal("failed save changed cached credentials")
	}
}
