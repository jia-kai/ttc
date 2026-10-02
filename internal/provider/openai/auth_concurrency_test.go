package openai

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"scicode/internal/filelock"
)

func TestSharedCredentialsRefreshOnceAndReloadRotatedTokens(t *testing.T) {
	var refreshes atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	a := adapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if refreshes.Add(1) == 1 {
			close(entered)
		}
		<-release
		io.WriteString(w, `{"access_token":"fresh","refresh_token":"rotated"}`)
	})
	a.credentials.Tokens.Access = "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`)) + ".signature"
	a.credentials.Tokens.Refresh = "original"
	if err := a.save(*a.credentials); err != nil {
		t.Fatal(err)
	}
	b := New(a.CredentialPath)
	b.Client, b.AuthURL = a.Client, a.AuthURL
	errors := make(chan error, 2)
	go func() { _, err := a.auth(context.Background()); errors <- err }()
	<-entered
	go func() { _, err := b.auth(context.Background()); errors <- err }()
	close(release)
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	if refreshes.Load() != 1 {
		t.Fatal("duplicate refresh", refreshes.Load())
	}
	if tokens, err := b.auth(context.Background()); err != nil || tokens.Access != "fresh" || tokens.Refresh != "rotated" {
		t.Fatal(tokens, err)
	}
	if err := a.saveLocked(context.Background(), Credentials{AuthMode: "chatgpt", Tokens: Tokens{Access: "replacement", AccountID: "other"}}); err != nil {
		t.Fatal(err)
	}
	if tokens, err := b.auth(context.Background()); err != nil || tokens.Access != "replacement" {
		t.Fatal("adapter cached another instance's obsolete login", tokens, err)
	}
}

func TestCrossProcessCredentialLockWaitCancels(t *testing.T) {
	a := adapterFixture(t, nil)
	a.credentials.Tokens.Access = "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`)) + ".signature"
	if err := a.save(*a.credentials); err != nil {
		t.Fatal(err)
	}
	lock, err := filelock.Acquire(context.Background(), a.CredentialPath+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := a.auth(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
