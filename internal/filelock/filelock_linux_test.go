package filelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"ttc/internal/prompts"
)

func TestDiagnosticAssetsExact(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{prompts.FileLockOpen, "open lock: %w"},
		{prompts.FileLockPrivateRegular, "lock must be a private 0600 regular file"},
		{prompts.FileLockAcquire, "acquire lock: %w"},
	} {
		if tc.got != tc.want {
			t.Fatalf("diagnostic changed: %q, want %q", tc.got, tc.want)
		}
	}
	cause := errors.New("cause")
	err := fmt.Errorf(prompts.FileLockAcquire, cause)
	if err.Error() != "acquire lock: cause" || !errors.Is(err, cause) {
		t.Fatalf("acquire wrapping changed: %v", err)
	}
}

func TestAcquireErrorsExactAndWrapped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "lock")
	_, err := Acquire(context.Background(), path)
	var cause *os.PathError
	if !errors.As(err, &cause) || !errors.Is(err, os.ErrNotExist) || err.Error() != "open lock: "+cause.Error() {
		t.Fatalf("open wrapping changed: %v", err)
	}
	path = filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	_, err = Acquire(context.Background(), path)
	if err == nil || err.Error() != "lock must be a private 0600 regular file" {
		t.Fatalf("unsafe lock: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Acquire(ctx, path); err != context.Canceled {
		t.Fatalf("cancellation identity changed: %v", err)
	}
}
