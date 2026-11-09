package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestTokenCheckpointValidation(t *testing.T) {
	valid := TokenCheckpoint{Version: 2, LocalInput: 0, Compatible: strings.Repeat("01", 32), Exact: strings.Repeat("ab", 32)}
	for _, version := range []int{1, 2} {
		checkpoint := valid
		checkpoint.Version = version
		if err := checkpoint.Validate(); err != nil {
			t.Fatal("supported checkpoint rejected", err)
		}
	}
	for _, edit := range []func(*TokenCheckpoint){
		func(c *TokenCheckpoint) { c.Version = 0 },
		func(c *TokenCheckpoint) { c.Version = 3 },
		func(c *TokenCheckpoint) { c.LocalInput = -1 },
		func(c *TokenCheckpoint) { c.Compatible = strings.Repeat("a", 63) },
		func(c *TokenCheckpoint) { c.Exact = strings.Repeat("z", 64) },
	} {
		checkpoint := valid
		edit(&checkpoint)
		if err := checkpoint.Validate(); err == nil {
			t.Fatal("invalid checkpoint accepted", checkpoint)
		}
	}
}

func TestEstimateInputCanonicalContentAndNativeFiles(t *testing.T) {
	message := Message{Type: TypeUserInput, Content: "hello", Files: []BinaryFile{{MIMEType: "image/png", Bytes: 90000}}}
	req := Request{System: "instructions", Messages: []Message{message}}
	result, err := EstimateInput(context.Background(), req)
	want := 4 + 16 + 2 + message.Files[0].EstimatedTokens()
	if err != nil || result.Input != want || !result.Estimated || result.Checkpoint != nil {
		t.Fatal("incorrect fallback estimate", result, want, err)
	}
	req.Messages[0] = Message{Type: TypeRuntimeInstruction, Content: "instruction"}
	result, err = EstimateInput(context.Background(), req)
	projected, _ := req.Messages[0].ModelContent()
	want = 4 + 16 + (len(projected)+2)/3
	if err != nil || result.Input != want {
		t.Fatal("fallback ignored model projection", result, want, err)
	}
}

func TestEstimateInputFailureIsNotUsable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := EstimateInput(ctx, Request{})
	if !errors.Is(err, context.Canceled) || result.Input != -1 || result.Checkpoint != nil {
		t.Fatal("canceled estimate was usable", result, err)
	}
	result, err = EstimateInput(context.Background(), Request{Messages: []Message{{}}})
	if err == nil || result.Input != -1 || result.Checkpoint != nil {
		t.Fatal("invalid estimate was usable", result, err)
	}
}

func TestEstimateMessagesPreservesSelectedReplay(t *testing.T) {
	state := &ReplayState{Provider: "provider", Model: "model", Version: 1, Items: []json.RawMessage{json.RawMessage(`{"native":"opaque"}`)}}
	message := Message{Type: TypeAssistant, Content: strings.Repeat("canonical text ", 100), State: state}
	got, err := EstimateMessages(context.Background(), []Message{message})
	if err != nil || got != ReplayTokens(state) {
		t.Fatal("message helper changed selected native replay occupancy", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := EstimateMessages(ctx, nil); !errors.Is(err, context.Canceled) || got != -1 {
		t.Fatal("message helper ignored canceled context", got, err)
	}
}
