package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ttc/internal/auth"
	"ttc/internal/filelock"
	"ttc/internal/llm"
	"ttc/internal/privatefile"
)

const clientID = "app_EMoamEEZ73f0CkXaXp7hrann"

const maxCredentialBytes = 1 << 20

// readCredentials bounds both the original file and concurrent growth, rejects
// final symlinks/special files, and optionally requires TTC's private file mode.
func readCredentials(path string, private bool) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("credential file must be a regular file")
	}
	if private && info.Mode().Perm() != 0600 {
		return nil, errors.New("credentials must be a private 0600 regular file")
	}
	if info.Size() > maxCredentialBytes {
		return nil, errors.New("credential file exceeds 1 MiB; select the subscription auth JSON file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxCredentialBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxCredentialBytes {
		return nil, errors.New("credential file exceeds 1 MiB; select the subscription auth JSON file")
	}
	return b, nil
}

// CredentialsTokens contains the complete subscription tokens persisted in the private auth store.
// Never log it or persist it in history.
type CredentialsTokens struct {
	ID        string `json:"id_token"`
	Access    string `json:"access_token"`
	Refresh   string `json:"refresh_token"`
	AccountID string `json:"account_id"`
}

// Credentials contains subscription tokens. Never log it or persist it in history.
type Credentials struct {
	AuthMode    string            `json:"auth_mode"`
	Tokens      CredentialsTokens `json:"tokens"`
	LastRefresh time.Time         `json:"last_refresh"`
}

// Authenticator serializes credential reads, rotation and replacement.
// Construct it with NewAuthenticator before use; it owns no inference or catalog state.
type Authenticator struct {
	Client         *http.Client
	AuthURL        string
	CredentialPath string
	authGate       chan struct{} // Serializes credential changes; waiters can cancel independently.
	credentials    *Credentials
}

// NewAuthenticator constructs an authenticator for an explicit private credential path.
func NewAuthenticator(path string) *Authenticator {
	return &Authenticator{Client: &http.Client{}, AuthURL: "https://auth.openai.com", CredentialPath: path, authGate: make(chan struct{}, 1)}
}

// lockAuth allows a canceled stream/child to leave while another caller is
// refreshing credentials or waiting for device authorization.
func (a *Authenticator) lockAuth(ctx context.Context) error {
	if a.authGate == nil {
		return errors.New("OpenAI authenticator must be constructed with NewAuthenticator")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case a.authGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-a.authGate
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Authenticator) unlockAuth() { <-a.authGate }
func (a *Authenticator) load() error {
	b, e := readCredentials(a.CredentialPath, true)
	if e != nil {
		if errors.Is(e, os.ErrNotExist) {
			return errors.New("subscription login required: run ttc --login or ttc --import-codex-auth \"$HOME/.codex/auth.json\"")
		}
		return fmt.Errorf("read subscription credentials: %w", e)
	}
	var c Credentials
	if e = json.Unmarshal(b, &c); e != nil {
		return errors.New("invalid subscription credential file")
	}
	if e = validateCredentials(c); e != nil {
		return e
	}
	a.credentials = &c
	return nil
}
func validateCredentials(c Credentials) error {
	if c.AuthMode != "chatgpt" || c.Tokens.Access == "" || c.Tokens.AccountID == "" {
		return errors.New("require ChatGPT subscription credentials; API-key billing is unsupported")
	}
	return nil
}
func (a *Authenticator) save(c Credentials) error {
	if e := validateCredentials(c); e != nil {
		return e
	}
	if e := privatefile.PrivateDir(filepath.Dir(a.CredentialPath)); e != nil {
		return e
	}
	b, e := json.Marshal(c)
	if e != nil {
		return e
	}
	if len(b) > maxCredentialBytes {
		return errors.New("credential file exceeds 1 MiB")
	}
	if e = privatefile.AtomicFile(a.CredentialPath, b, 0600); e != nil {
		return e
	}
	a.credentials = &c
	return nil
}

// ImportCodex copies explicitly authorized subscription credentials into private TTC storage.
// It never changes the source, accepts API keys, or initiates a token refresh.
func (a *Authenticator) ImportCodex(ctx context.Context, path string) error {
	if err := a.lockAuth(ctx); err != nil {
		return err
	}
	defer a.unlockAuth()
	b, e := readCredentials(path, false)
	if e != nil {
		return e
	}
	var source struct {
		AuthMode    string            `json:"auth_mode"`
		APIKey      *string           `json:"OPENAI_API_KEY"`
		Tokens      CredentialsTokens `json:"tokens"`
		LastRefresh time.Time         `json:"last_refresh"`
	}
	if e = json.Unmarshal(b, &source); e != nil {
		return errors.New("invalid Codex credential file")
	}
	if source.APIKey != nil && *source.APIKey != "" {
		return errors.New("API-key import is unsupported")
	}
	return a.saveLocked(ctx, Credentials{source.AuthMode, source.Tokens, source.LastRefresh})
}

// saveLocked coordinates only the credential replacement, never device authorization.
// The caller owns authGate.
func (a *Authenticator) saveLocked(ctx context.Context, c Credentials) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := privatefile.PrivateDir(filepath.Dir(a.CredentialPath)); err != nil {
		return err
	}
	lock, err := filelock.Acquire(ctx, a.CredentialPath+".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.save(c)
}
func expiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return time.Time{}
	}
	var v struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &v) != nil {
		return time.Time{}
	}
	return time.Unix(v.Exp, 0)
}

// AccessTokens reloads local credentials and refreshes expired tokens under a shared file lock.
// It returns only the access token and account ID required by the transport.
func (a *Authenticator) AccessTokens(ctx context.Context) (AccessTokens, error) {
	if err := a.lockAuth(ctx); err != nil {
		return AccessTokens{}, err
	}
	defer a.unlockAuth()
	if e := a.load(); e != nil {
		return AccessTokens{}, e
	}
	if err := ctx.Err(); err != nil {
		return AccessTokens{}, err
	}
	c := *a.credentials
	exp := expiry(c.Tokens.Access)
	if exp.IsZero() || time.Now().Add(time.Minute).Before(exp) {
		return AccessTokens{Access: c.Tokens.Access, AccountID: c.Tokens.AccountID}, nil
	}
	lock, err := filelock.Acquire(ctx, a.CredentialPath+".lock")
	if err != nil {
		return AccessTokens{}, err
	}
	defer lock.Close()
	// Another instance may already have refreshed or replaced the login.
	if err := a.load(); err != nil {
		return AccessTokens{}, err
	}
	if err := ctx.Err(); err != nil {
		return AccessTokens{}, err
	}
	c = *a.credentials
	exp = expiry(c.Tokens.Access)
	if exp.IsZero() || time.Now().Add(time.Minute).Before(exp) {
		return AccessTokens{Access: c.Tokens.Access, AccountID: c.Tokens.AccountID}, nil
	}
	if c.Tokens.Refresh == "" {
		return AccessTokens{}, errors.New("subscription token expired; login required")
	}
	var response struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		ID      string `json:"id_token"`
	}
	_, e := a.json(ctx, a.AuthURL+"/oauth/token", map[string]string{"grant_type": "refresh_token", "client_id": clientID, "refresh_token": c.Tokens.Refresh}, &response)
	if e != nil {
		return AccessTokens{}, e
	}
	if response.Access == "" {
		return AccessTokens{}, errors.New("refresh returned no access token")
	}
	c.Tokens.Access = response.Access
	if response.Refresh != "" {
		c.Tokens.Refresh = response.Refresh
	}
	if response.ID != "" {
		c.Tokens.ID = response.ID
	}
	c.LastRefresh = time.Now().UTC()
	if e = a.save(c); e != nil {
		return AccessTokens{}, e
	}
	return AccessTokens{Access: c.Tokens.Access, AccountID: c.Tokens.AccountID}, nil
}
func (a *Authenticator) json(ctx context.Context, endpoint string, body any, out any) (int, error) {
	b, e := json.Marshal(body)
	if e != nil {
		return 0, e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(b)))
	if e != nil {
		return 0, e
	}
	req.Header.Set("Content-Type", "application/json")
	resp, e := a.Client.Do(req)
	if e != nil {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if invalidCertificate(e) {
			return 0, errors.New("authentication TLS certificate verification failed")
		}
		return 0, &llm.TransientError{Err: errors.New("authentication transport failed")}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("authentication HTTP %d", resp.StatusCode)
		if resp.StatusCode == 429 || resp.StatusCode >= 500 && resp.StatusCode <= 599 {
			return resp.StatusCode, &llm.TransientError{Err: err}
		}
		return resp.StatusCode, err
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); e != nil {
		if err := ctx.Err(); err != nil {
			return resp.StatusCode, err
		}
		return resp.StatusCode, errors.New("invalid authentication response")
	}
	return resp.StatusCode, nil
}

// Login emits a typed code step, polls at the prescribed interval, and privately saves tokens.
func (a *Authenticator) Login(ctx context.Context, ui auth.UI) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if err := a.lockAuth(ctx); err != nil {
		return err
	}
	defer a.unlockAuth()
	if ui == nil {
		return errors.New("device login requires an authorization UI")
	}
	var code struct {
		DeviceID string          `json:"device_auth_id"`
		UserCode string          `json:"user_code"`
		Interval json.RawMessage `json:"interval"`
	}
	if _, e := a.json(ctx, a.AuthURL+"/api/accounts/deviceauth/usercode", map[string]string{"client_id": clientID}, &code); e != nil {
		return e
	}
	if code.DeviceID == "" || code.UserCode == "" {
		return errors.New("device login returned incomplete code")
	}
	interval := 5
	var s string
	if json.Unmarshal(code.Interval, &s) == nil {
		n, e := strconv.Atoi(s)
		if e != nil || n <= 0 || n > 300 {
			return errors.New("invalid login interval")
		}
		interval = n
	} else if len(code.Interval) > 0 {
		if e := json.Unmarshal(code.Interval, &interval); e != nil || interval <= 0 || interval > 300 {
			return errors.New("invalid login interval")
		}
	}
	if e := ui.Present(ctx, auth.Step{Kind: "device_code", URL: a.AuthURL + "/codex/device", Code: code.UserCode, ExpiresSeconds: 900}); e != nil {
		return e
	}
	for {
		if e := wait(ctx, time.Duration(interval)*time.Second); e != nil {
			return e
		}
		var poll struct {
			Code     string `json:"authorization_code"`
			Verifier string `json:"code_verifier"`
		}
		status, e := a.json(ctx, a.AuthURL+"/api/accounts/deviceauth/token", map[string]string{"device_auth_id": code.DeviceID, "user_code": code.UserCode}, &poll)
		if status == 403 || status == 404 {
			continue
		}
		if status == 429 {
			interval += 5
			if interval > 300 {
				return errors.New("login polling repeatedly slowed down")
			}
			continue
		}
		if e != nil {
			return e
		}
		if poll.Code == "" || poll.Verifier == "" {
			return errors.New("invalid device authorization response")
		}
		form := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {poll.Code}, "redirect_uri": {a.AuthURL + "/deviceauth/callback"}, "code_verifier": {poll.Verifier}}
		req, e := http.NewRequestWithContext(ctx, "POST", a.AuthURL+"/oauth/token", strings.NewReader(form.Encode()))
		if e != nil {
			return e
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, e := a.Client.Do(req)
		if e != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			return errors.New("token exchange transport failed")
		}
		var tokens struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
			ID      string `json:"id_token"`
		}
		e = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tokens)
		resp.Body.Close()
		if err := ctx.Err(); err != nil {
			return err
		}
		if resp.StatusCode != 200 || e != nil {
			return fmt.Errorf("token exchange failed (HTTP %d)", resp.StatusCode)
		}
		account := accountID(tokens.ID)
		if account == "" {
			account = accountID(tokens.Access)
		}
		if e = a.saveLocked(ctx, Credentials{AuthMode: "chatgpt", Tokens: CredentialsTokens{ID: tokens.ID, Access: tokens.Access, Refresh: tokens.Refresh, AccountID: account}, LastRefresh: time.Now().UTC()}); e != nil {
			return e
		}
		e = ui.Present(ctx, auth.Step{Kind: "complete", Message: "Subscription login complete"})
		return e
	}
}
func accountID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			Account string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(b, &claims) != nil {
		return ""
	}
	return claims.Auth.Account
}
