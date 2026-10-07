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

	"golang.org/x/net/http/httpguts"

	"ttc/internal/auth"
	"ttc/internal/filelock"
	"ttc/internal/llm"
	"ttc/internal/privatefile"
	"ttc/internal/prompts"
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
		return nil, errors.New(prompts.OpenAICredentialRegularFileRequired)
	}
	if private && info.Mode().Perm() != 0600 {
		return nil, errors.New(prompts.OpenAICredentialPrivateFileRequired)
	}
	if info.Size() > maxCredentialBytes {
		return nil, errors.New(prompts.OpenAICredentialFileLimitGuidance)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxCredentialBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxCredentialBytes {
		return nil, errors.New(prompts.OpenAICredentialFileLimitGuidance)
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
		return errors.New(prompts.OpenAIAuthenticatorConstructorRequired)
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
			return errors.New(prompts.OpenAISubscriptionLoginRequired)
		}
		return fmt.Errorf(prompts.OpenAIReadCredentials, e)
	}
	var c Credentials
	if e = json.Unmarshal(b, &c); e != nil {
		return errors.New(prompts.OpenAIInvalidCredentialFile)
	}
	if e = validateCredentials(c); e != nil {
		return e
	}
	a.credentials = &c
	return nil
}
func validateCredentials(c Credentials) error {
	if c.AuthMode != "chatgpt" || c.Tokens.Access == "" || c.Tokens.AccountID == "" {
		return errors.New(prompts.OpenAISubscriptionCredentialsRequired)
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
		return errors.New(prompts.OpenAICredentialFileLimit)
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
// Remote refresh failures are transient; local storage/configuration failures,
// TLS certificate verification and cancellation remain final.
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
		return AccessTokens{}, errors.New(prompts.OpenAITokenExpired)
	}
	var response struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		ID      string `json:"id_token"`
	}
	result, e := a.json(ctx, a.AuthURL+"/oauth/token", map[string]string{"grant_type": "refresh_token", "client_id": clientID, "refresh_token": c.Tokens.Refresh}, &response)
	if e != nil {
		var upstream *authUpstreamFailure
		if errors.As(e, &upstream) {
			e = &llm.TransientError{Err: e}
		}
		return AccessTokens{}, e
	}
	if response.Access == "" {
		return AccessTokens{}, &llm.TransientError{Err: result.failure(fmt.Errorf(prompts.OpenAIRefreshMissingAccessToken, result.status))}
	}
	c.Tokens.Access = response.Access
	if response.Refresh != "" {
		c.Tokens.Refresh = response.Refresh
	}
	if response.ID != "" {
		c.Tokens.ID = response.ID
	}
	c.LastRefresh = time.Now().UTC()
	if err := ctx.Err(); err != nil {
		return AccessTokens{}, err
	}
	if e = a.save(c); e != nil {
		return AccessTokens{}, e
	}
	return AccessTokens{Access: c.Tokens.Access, AccountID: c.Tokens.AccountID}, nil
}

// authUpstreamFailure records remote origin without granting Login retry policy.
// err contains only bounded, safe diagnostics, never raw response or transport errors.
type authUpstreamFailure struct {
	err        error
	retryAfter string
}

func (e *authUpstreamFailure) Error() string { return e.err.Error() }
func (e *authUpstreamFailure) Unwrap() error { return e.err }

// credentialRetryAfter finds refresh response hints through Stream's wrappers.
func credentialRetryAfter(err error) string {
	var upstream *authUpstreamFailure
	if errors.As(err, &upstream) {
		return upstream.retryAfter
	}
	return ""
}

type authJSONResult struct {
	status     int
	requestID  string
	retryAfter string
}

func (r authJSONResult) failure(err error) *authUpstreamFailure {
	diagnostic := errors.New(safeFailureText(err.Error(), failureDetailsLimit))
	return &authUpstreamFailure{err: failureWithRequestID(diagnostic, r.requestID), retryAfter: r.retryAfter}
}

// authHTTPFailure exposes only OAuth/JSON error fields, not token fields or
// arbitrary bodies. The caller bounds raw before invoking this helper.
func authHTTPFailure(status int, raw []byte, readErr error, redact func(string) string) error {
	detail := prompts.OpenAIErrorDetailsMissing
	if len(raw) > failureBodyLimit {
		detail += prompts.OpenAIBodyTruncatedSuffix
	} else if root := failureObject(raw); root != nil {
		var parts []string
		if code := failureString(root["error"]); code != "" {
			parts = append(parts, "code="+safeFailureText(redact(code), 256))
		}
		if message := failureString(root["error_description"]); message != "" {
			parts = append(parts, "message="+safeFailureText(redact(message), 2048))
		}
		for _, object := range []map[string]json.RawMessage{failureObject(root["error"]), root} {
			for _, field := range []string{"type", "code", "message", "param"} {
				if value := failureString(object[field]); value != "" {
					limit := 256
					if field == "message" {
						limit = 2048
					}
					parts = append(parts, field+"="+safeFailureText(redact(value), limit))
				}
			}
		}
		if len(parts) > 0 {
			detail = strings.Join(parts, "; ")
		}
	} else if len(raw) == 0 {
		detail += prompts.OpenAIBodyAbsentSuffix
	} else {
		detail += prompts.OpenAIBodyInvalidJSONSuffix
	}
	if readErr != nil {
		detail += prompts.OpenAIBodyReadFailedSuffix
	}
	return fmt.Errorf(prompts.OpenAIAuthResponseHTTP, status, safeFailureText(detail, failureDetailsLimit))
}

// redactAuthTokens protects known token values even when an upstream places them
// in an otherwise allowed error field or request-ID header.
func (a *Authenticator) redactAuthTokens(text string, payloads ...[]byte) string {
	var tokens []string
	if a.credentials != nil {
		tokens = append(tokens, a.credentials.Tokens.Access, a.credentials.Tokens.Refresh, a.credentials.Tokens.ID)
	}
	for _, raw := range payloads {
		root := failureObject(raw)
		for _, field := range []string{"access_token", "refresh_token", "id_token"} {
			tokens = append(tokens, failureString(root[field]))
		}
	}
	for _, token := range tokens {
		if token != "" {
			text = strings.ReplaceAll(text, token, prompts.OpenAIDiagnosticRedacted)
		}
	}
	return text
}

func (a *Authenticator) json(ctx context.Context, endpoint string, body any, out any) (authJSONResult, error) {
	var result authJSONResult
	b, e := json.Marshal(body)
	if e != nil {
		return result, e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(string(b)))
	if e != nil {
		return result, errors.New(prompts.OpenAIInvalidAuthEndpoint)
	}
	if a.Client == nil || req.URL.Host == "" || !httpguts.ValidHostHeader(req.URL.Host) || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		return result, errors.New(prompts.OpenAIInvalidAuthHTTPConfiguration)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, e := a.Client.Do(req)
	if e != nil {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if errors.Is(e, context.Canceled) {
			return result, context.Canceled
		}
		if invalidCertificate(e) {
			return result, errors.New(prompts.OpenAIAuthCertificateFailed)
		}
		// Client.Do returns a response with an error only when redirect policy
		// rejects the request. Repeating that local policy cannot recover.
		if resp != nil {
			return result, errors.New(prompts.OpenAIAuthRedirectPolicyFailed)
		}
		return result, result.failure(errors.New(prompts.OpenAIAuthTransportFailed))
	}
	defer resp.Body.Close()
	result = authJSONResult{status: resp.StatusCode, requestID: resp.Header.Get("x-request-id"), retryAfter: resp.Header.Get("Retry-After")}
	result.requestID = a.redactAuthTokens(result.requestID, b)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, failureBodyLimit+1))
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if errors.Is(readErr, context.Canceled) {
			return result, context.Canceled
		}
		if invalidCertificate(readErr) {
			return result, errors.New(prompts.OpenAIAuthCertificateFailed)
		}
		diagnostic := authHTTPFailure(resp.StatusCode, raw, readErr, func(text string) string { return a.redactAuthTokens(text, b, raw) })
		result.requestID = a.redactAuthTokens(result.requestID, raw)
		return result, result.failure(diagnostic)
	}
	raw, e := io.ReadAll(io.LimitReader(resp.Body, maxCredentialBytes+1))
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if errors.Is(e, context.Canceled) {
		return result, context.Canceled
	}
	if invalidCertificate(e) {
		return result, errors.New(prompts.OpenAIAuthCertificateFailed)
	}
	result.requestID = a.redactAuthTokens(result.requestID, raw)
	if e != nil || len(raw) > maxCredentialBytes || json.Unmarshal(raw, out) != nil {
		return result, result.failure(fmt.Errorf(prompts.OpenAIInvalidAuthResponse, resp.StatusCode))
	}
	return result, nil
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
		result, e := a.json(ctx, a.AuthURL+"/api/accounts/deviceauth/token", map[string]string{"device_auth_id": code.DeviceID, "user_code": code.UserCode}, &poll)
		status := result.status
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
