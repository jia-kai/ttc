package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"ttc/internal/prompts"
)

// InputEstimate reports complete request input occupancy in tokens. Estimated is
// false only when the provider measured this exact input. Checkpoint is optional,
// payload-free metadata to bind to a successful response's usable Usage.
// When Prepared is nonnil, pass it to Stream so attachment availability and
// other projection-time inputs cannot change the checkpoint's wire identity.
// On error, Input is -1 and Checkpoint and Prepared are nil.
type InputEstimate struct {
	Input      int
	Estimated  bool
	Checkpoint *TokenCheckpoint
	Prepared   PreparedInput // Optional immutable preparation to pass to the matching Stream request.
}

// PreparedInput is a provider-owned immutable native request preparation. Its
// concrete contents are opaque outside its provider. It may own input payloads
// for one request's lifetime; it is never persisted or retained by token caches.
// Supply it only with the same input and generation options used to estimate it.
// Providers reject foreign handles and input changes rather than sending stale
// data. Request transport callbacks and retry bookkeeping may change independently.
type PreparedInput interface {
	Provider() string
}

// TokenCheckpoint is versioned provider-owned accounting metadata, not input
// payload or usage. Version 1 is a readable legacy format; version 2 identifies
// a native provider configuration and a chained exact input prefix.
type TokenCheckpoint struct {
	Version    int    `json:"version"`
	LocalInput int    `json:"local_input"` // Local occupancy in tokens, before measured-prefix substitution.
	Compatible string `json:"compatible"`  // Hex SHA-256 of the provider's input configuration.
	Exact      string `json:"exact"`       // Hex SHA-256 identifying the complete prepared input.
}

// Validate checks supported versions, nonnegative token counts and SHA-256
// encodings. It does not establish provider identity or successful usage ownership.
func (c TokenCheckpoint) Validate() error {
	if (c.Version != 1 && c.Version != 2) || c.LocalInput < 0 {
		return errors.New(prompts.LLMInvalidTokenCheckpoint)
	}
	for _, text := range []string{c.Compatible, c.Exact} {
		decoded, err := hex.DecodeString(text)
		if err != nil || len(decoded) != sha256.Size {
			return errors.New(prompts.LLMInvalidTokenFingerprint)
		}
	}
	return nil
}

// EstimateInput estimates canonical request content without provider
// calibration or transport encoding. Native attachments use format reserves,
// never base64 size. Callers own any retained-input projection. No checkpoint is
// generated because only a provider can identify its exact native input.
func EstimateInput(ctx context.Context, req Request) (InputEstimate, error) {
	invalid := InputEstimate{Input: -1, Estimated: true}
	if err := ctx.Err(); err != nil {
		return invalid, err
	}
	n, err := EstimateMessages(ctx, ContextFor(req.Selection, req.Messages))
	if err != nil {
		return invalid, err
	}
	if err := addEstimatedTokens(&n, estimatedTextTokens(req.System)); err != nil {
		return invalid, err
	}
	for _, tool := range req.Tools {
		for _, count := range []int{16, estimatedTextTokens(tool.Name), estimatedTextTokens(tool.Description), estimatedTextTokens(string(tool.Parameters))} {
			if err := addEstimatedTokens(&n, count); err != nil {
				return invalid, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return invalid, err
	}
	return InputEstimate{Input: n, Estimated: true}, nil
}

// EstimateMessages conservatively counts an already selected and projected
// message sequence. Matching replay state replaces canonical text/calls, and
// attachments use native format reserves. The caller owns provider selection
// and retained-input projection. Errors return -1 rather than a partial count.
func EstimateMessages(ctx context.Context, messages []Message) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	n := 0
	for i, message := range messages {
		if err := ctx.Err(); err != nil {
			return -1, err
		}
		content, err := message.ModelContent()
		if err != nil {
			return -1, fmt.Errorf(prompts.LLMEstimateInputMessage, i, err)
		}
		if message.State != nil {
			if err := addEstimatedTokens(&n, ReplayTokens(message.State)); err != nil {
				return -1, err
			}
			continue
		}
		for _, count := range []int{16, estimatedTextTokens(content)} {
			if err := addEstimatedTokens(&n, count); err != nil {
				return -1, err
			}
		}
		for _, call := range message.Calls {
			for _, count := range []int{16, estimatedTextTokens(call.Name), estimatedTextTokens(string(call.Arguments))} {
				if err := addEstimatedTokens(&n, count); err != nil {
					return -1, err
				}
			}
		}
		for _, file := range message.Files {
			if err := addEstimatedTokens(&n, file.EstimatedTokens()); err != nil {
				return -1, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	return n, nil
}

func estimatedTextTokens(text string) int { return len(text)/3 + (len(text)%3+2)/3 }

func addEstimatedTokens(total *int, count int) error {
	if count < 0 || count > int(^uint(0)>>1)-*total {
		return errors.New(prompts.LLMInputTokenOverflow)
	}
	*total += count
	return nil
}
