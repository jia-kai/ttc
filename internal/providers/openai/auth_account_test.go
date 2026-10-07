package openai

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"ttc/internal/filelock"
)

func TestAuthenticationFailuresStayLocal(t *testing.T) {
	dir := t.TempDir()
	a := NewAuthenticator(filepath.Join(dir, "missing.json"))
	if _, err := a.AccessTokens(context.Background()); err == nil || !strings.Contains(err.Error(), "ttc --login") {
		t.Fatal("missing credentials must offer startup command", err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	a.CredentialPath = filepath.Join(file, "auth.json")
	_, err := a.AccessTokens(context.Background())
	var pathError *os.PathError
	if !errors.As(err, &pathError) {
		t.Fatal("credential filesystem error hidden", err)
	}
}

func TestAccountGuardReloadsUnderSharedCredentialLock(t *testing.T) {
	a := authenticatorFixture(t, nil)
	b := NewAuthenticator(a.CredentialPath)
	lock, err := filelock.Acquire(context.Background(), a.CredentialPath+".lock")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	committed := false
	if err := a.WithAccount(ctx, "account", func() error { committed = true; return nil }); !errors.Is(err, context.DeadlineExceeded) || committed {
		t.Fatal(err, committed)
	}
	lock.Close()
	if err := b.saveLocked(context.Background(), Credentials{AuthMode: "chatgpt", Tokens: CredentialsTokens{Access: "other", AccountID: "other"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.WithAccount(context.Background(), "account", func() error { committed = true; return nil }); err == nil || committed {
		t.Fatal("published obsolete account data", err, committed)
	}
}
