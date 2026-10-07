package openai

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func authenticatorFixture(t *testing.T, h http.HandlerFunc) *Authenticator {
	t.Helper()
	a := NewAuthenticator(filepath.Join(t.TempDir(), "private", "auth.json"))
	a.AuthURL = "https://mock.test"
	a.Client = &http.Client{Transport: handlerTransport(h)}
	if err := a.save(Credentials{AuthMode: "chatgpt", Tokens: CredentialsTokens{Access: "token", AccountID: "account"}, LastRefresh: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return a
}
func transportAdapter(a *Authenticator) *Adapter {
	return NewAdapter(Config{Client: a.Client, BaseURL: a.AuthURL, TokenSource: a.AccessTokens})
}

func TestCurrentAndAccountGuard(t *testing.T) {
	a := authenticatorFixture(t, func(http.ResponseWriter, *http.Request) { t.Fatal("local account operations sent HTTP") })
	expected := *a.credentials
	expected.LastRefresh = expected.LastRefresh.UTC()
	expected.Tokens.ID = "private-id"
	expected.Tokens.Refresh = "private-refresh"
	if err := a.save(expected); err != nil {
		t.Fatal(err)
	}
	c, err := a.Current(context.Background())
	if err != nil || c != expected {
		t.Fatal(c, err)
	}
	c.Tokens.AccountID = "changed-copy"
	if tokens, err := a.AccessTokens(context.Background()); err != nil || tokens != (AccessTokens{Access: "token", AccountID: "account"}) {
		t.Fatal("transport credentials did not preserve account identity", err)
	}
	calls := 0
	if err := a.WithAccount(context.Background(), "other", func() error { calls++; return nil }); err == nil || calls != 0 {
		t.Fatal(err, calls)
	}
	if err := a.WithAccount(context.Background(), "account", func() error { calls++; return nil }); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	if current, err := a.Current(context.Background()); err != nil || current != expected {
		t.Fatal("account guard changed stored auth credentials", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.ImportCodex(ctx, "missing"); err != context.Canceled {
		t.Fatal(err)
	}
}
