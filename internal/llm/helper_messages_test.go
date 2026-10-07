package llm

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"ttc/internal/prompts"
)

func TestUnavailableBinaryFileUsesPromptAssetAndUnwraps(t *testing.T) {
	cause := errors.New("source checksum mismatch")
	failure := &UnavailableBinaryFileError{File: BinaryFile{Path: "/source.pdf", SHA256: "checksum"}, Err: cause}
	if got, want := failure.Error(), fmt.Sprintf(prompts.BinaryUnavailable, failure.File.Path, failure.File.SHA256, cause); got != want {
		t.Fatalf("notice = %q, want %q", got, want)
	}
	if !errors.Is(failure, cause) {
		t.Fatal("unavailable-file notice lost its source error")
	}
}

func TestProviderNeutralDiagnosticBytes(t *testing.T) {
	var message Message
	if err := message.AppendState(Selection{}, 1, []byte(`{}`)); err == nil || err.Error() != "invalid provider replay state" {
		t.Fatalf("invalid replay state error = %v", err)
	}
	selection := Selection{Provider: "fixture", Model: ModelSpec{ID: "model"}}
	if err := message.AppendState(selection, 1, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := message.AppendState(selection, 2, []byte(`{}`)); err == nil || err.Error() != "mixed provider replay state" {
		t.Fatalf("mixed replay state error = %v", err)
	}
	script := &Script{}
	if err := script.Stream(context.Background(), Request{}, nil); err == nil || err.Error() != "offline script exhausted" {
		t.Fatalf("exhausted script error = %v", err)
	}
	script = &Script{Responses: []ScriptResponse{{Prefix: "fixture prefix"}}}
	if err := script.Stream(context.Background(), Request{}, nil); err == nil || err.Error() != "offline script prefix mismatch: expected fixture prefix" {
		t.Fatalf("script prefix error = %v", err)
	}
}
