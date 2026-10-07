package openai

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"ttc/internal/auth"
)

func TestExplicitImportRejectsAPIKeysAndPrivateCredentials(t *testing.T) {
	a := authenticatorFixture(t, func(http.ResponseWriter, *http.Request) {})
	p := filepath.Join(t.TempDir(), "codex.json")
	os.WriteFile(p, []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"secret"}`), 0600)
	if err := a.ImportCodex(context.Background(), p); err == nil {
		t.Fatal("accepted API billing")
	}
	os.WriteFile(p, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"imported","account_id":"account"}}`), 0600)
	if err := a.ImportCodex(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(a.CredentialPath)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal(st, err)
	}
}

type testLogin struct{ steps []auth.Step }

func (u *testLogin) Present(ctx context.Context, s auth.Step) error {
	u.steps = append(u.steps, s)
	return nil
}
func TestDeviceLoginAndCancellation(t *testing.T) {
	claim := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account"}}`))
	a := authenticatorFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			fmt.Fprint(w, `{"device_auth_id":"d","user_code":"CODE","interval":"1"}`)
		case "/api/accounts/deviceauth/token":
			fmt.Fprint(w, `{"authorization_code":"authorized","code_verifier":"verify"}`)
		case "/oauth/token":
			if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "authorization_code" {
				t.Error("wrong exchange")
			}
			fmt.Fprintf(w, `{"access_token":"a","refresh_token":"r","id_token":"x.%s.y"}`, claim)
		default:
			t.Error(r.URL.Path)
		}
	})
	ui := &testLogin{}
	if err := a.Login(context.Background(), ui); err != nil {
		t.Fatal(err)
	}
	if len(ui.steps) != 2 || ui.steps[0].Code != "CODE" {
		t.Fatal(ui.steps)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Login(ctx, &testLogin{}); err == nil {
		t.Fatal("ignored canceled login")
	}
}
