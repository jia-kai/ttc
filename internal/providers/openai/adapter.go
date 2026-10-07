package openai

import (
	"context"
	"errors"
	"net/http"
	"ttc/internal/llm"
)

// AccessTokens contains only the access token and account identity required for
// a transport operation, never ID or refresh tokens. Never log its fields.
type AccessTokens struct {
	Access    string `json:"access_token"`
	AccountID string `json:"account_id"`
}

// Config injects context-bound access tokens and original binary resolution.
// Nil Client and empty BaseURL use the standard HTTP client and subscription endpoint.
// TokenSource is required; ResolveBinary is required only for binary attachments.
type Config struct {
	Client        *http.Client
	BaseURL       string
	TokenSource   func(context.Context) (AccessTokens, error)
	ResolveBinary func(context.Context, llm.BinaryFile) (llm.BinaryPayload, error)
}

// Adapter owns only HTTP protocol and codecs. Dependencies must remain valid for
// its lifetime and support concurrent calls if the adapter is used concurrently.
type Adapter struct {
	Client        *http.Client
	BaseURL       string
	tokenSource   func(context.Context) (AccessTokens, error)
	resolveBinary func(context.Context, llm.BinaryFile) (llm.BinaryPayload, error)
}

// NewAdapter constructs a transport without reading credentials or application files.
func NewAdapter(c Config) *Adapter {
	if c.Client == nil {
		c.Client = &http.Client{}
	}
	if c.BaseURL == "" {
		c.BaseURL = "https://chatgpt.com/backend-api/codex"
	}
	return &Adapter{Client: c.Client, BaseURL: c.BaseURL, tokenSource: c.TokenSource, resolveBinary: c.ResolveBinary}
}

func (a *Adapter) credentials(ctx context.Context) (AccessTokens, error) {
	if err := ctx.Err(); err != nil {
		return AccessTokens{}, err
	}
	if a.tokenSource == nil {
		return AccessTokens{}, errors.New("OpenAI transport requires a credentials source")
	}
	tokens, err := a.tokenSource(ctx)
	if err != nil {
		return AccessTokens{}, err
	}
	if err := ctx.Err(); err != nil {
		return AccessTokens{}, err
	}
	if tokens.Access == "" || tokens.AccountID == "" {
		return AccessTokens{}, errors.New("OpenAI credentials require access token and account ID")
	}
	return tokens, nil
}
