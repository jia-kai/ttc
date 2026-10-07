package jobs

import (
	"context"
	"errors"
	"testing"

	"ttc/internal/capture"
	"ttc/internal/lsp"
	"ttc/internal/prompts"
)

func TestJobValidationUsesPromptAssetsWithoutProcesses(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	if _, err := m.View("main", "missing"); !errors.Is(err, ErrNotFound) || err.Error() != prompts.JobNotFound {
		t.Fatalf("missing-job sentinel changed: %v", err)
	}
	if _, err := m.Start("main", "", "", -1, false, false, false); err == nil || err.Error() != prompts.JobTimeoutNonnegative {
		t.Fatalf("timeout guidance = %v", err)
	}
	// Synthetic completed handles exercise validation without launching a shell
	// or language server. Close can join an already-closed done channel.
	done := make(chan struct{})
	close(done)
	j := &job{view: Snapshot{ID: "test", Kind: "shell", Owner: "main", Status: "completed"}, done: done, cancel: func() {}, stdout: m.pool.NewBuffer(capture.CallLimit / 2), stderr: m.pool.NewBuffer(capture.CallLimit / 2)}
	m.jobs[j.view.ID] = j
	checkLSP := func(id, code, want string) {
		t.Helper()
		_, err := m.QueryLSP(context.Background(), "main", id, lsp.Query{})
		var failure *lsp.Error
		if !errors.As(err, &failure) || failure.Code != code || failure.Message != want {
			t.Fatalf("LSP validation = %v, want %s: %q", err, code, want)
		}
	}
	checkLSP("missing", "not_found", prompts.JobLSPNotFound)
	checkLSP("test", "invalid_input", prompts.JobNotLSPServer)
	j.client = &lsp.Client{}
	checkLSP("test", "job_not_running", prompts.JobLSPExited)
	_, err := m.Read(context.Background(), "main", "test", ReadOptions{Stream: "stdout", Limit: 4})
	var failure *lsp.Error
	if !errors.As(err, &failure) || failure.Code != "invalid_input" || failure.Message != prompts.JobLSPProtocolStdout {
		t.Fatalf("protocol stdout guidance = %v", err)
	}
	j.client = nil
	if _, err := m.Read(context.Background(), "main", "test", ReadOptions{Stream: "invalid", Limit: 4}); err == nil || err.Error() != prompts.JobInvalidStream {
		t.Fatalf("stream validation = %v", err)
	}
	j.view.Kind = "subagent"
	if _, err := m.Stop("main", "test"); err == nil || err.Error() != prompts.JobCannotStopOwnTask {
		t.Fatalf("self-stop guidance = %v", err)
	}
}
