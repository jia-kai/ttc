package tool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"ttc/internal/prompts"
	"ttc/internal/render"
	"ttc/internal/workspace"
)

// fileResult separates mutation presentation from the model-facing value.
// Diffs are captured once from immutable snapshots, never from live files.
type fileResult struct {
	value           any
	summary, detail string
}

// mutationResult preserves actual filesystem effects even when recording history
// failed. A committed ID is not evidence that a particular path was applied.
func mutationResult(ctx context.Context, value any, res workspace.Result, err error, patch bool) (fileResult, error) {
	if err != nil {
		applied, remaining := []string{}, []string{}
		for _, change := range res.Changes {
			if change.Applied {
				applied = append(applied, change.Path)
			} else {
				remaining = append(remaining, change.Path)
			}
		}
		if len(applied) != 0 {
			code := "execution_failed"
			if errors.Is(err, context.Canceled) {
				code = "cancelled"
			}
			if errors.Is(err, context.DeadlineExceeded) {
				code = "timeout"
			}
			if patch {
				code = "partial_patch"
			}
			details := map[string]any{
				"applied": applied, "remaining": remaining,
				"history_recorded": res.HistoryRecorded,
				"undoable":         res.HistoryRecorded && res.Reversible,
				"retry_safe":       false,
			}
			message := fmt.Sprintf(prompts.ToolAppliedMutation, err)
			if res.HistoryRecorded {
				details["change_id"] = res.ChangeID
			} else {
				message += prompts.ToolMutationHistoryMissing
			}
			err = &Error{Code: code, Message: message, Details: details}
		}
	}
	return presentFiles(ctx, value, res.Changes), err
}

type diffCapture struct {
	buffer    bytes.Buffer // Do not embed: promoted ReadFrom would bypass Write's budget through io.Copy.
	remaining *int         // Shared byte budget for every changed path in one tool call.
	truncated bool
}

func (w *diffCapture) Write(p []byte) (int, error) {
	n := min(len(p), *w.remaining)
	w.buffer.Write(p[:n])
	*w.remaining -= n
	w.truncated = w.truncated || n < len(p)
	return len(p), nil // Drain discarded output until the process exits or times out.
}

func presentFiles(ctx context.Context, value any, changes []workspace.PathChange) fileResult {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	remaining := 256 << 10
	var detail strings.Builder
	var preview []string
	for _, change := range changes {
		if !change.Applied {
			continue
		}
		path := render.Clean(change.Path)
		detail.WriteString("### " + render.EscapeInline(path) + "\n\n")
		before, after := "/dev/null", "/dev/null"
		if change.Before.Exists {
			before = change.Before.Blob
		}
		if change.After.Exists {
			after = change.After.Blob
		}
		diff := ""
		note := ""
		if remaining == 0 || ctx.Err() != nil {
			note = prompts.ToolDiffBudgetExhausted
		} else {
			capture := &diffCapture{remaining: &remaining}
			cmd := exec.CommandContext(ctx, "diff", "-u", "--label", path, "--label", path, "--", before, after)
			cmd.Stdout, cmd.Stderr = capture, capture
			cmd.WaitDelay = 100 * time.Millisecond
			err := cmd.Run()
			var exit *exec.ExitError
			if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
				note = prompts.ToolDiffUnavailable + err.Error()
			} else {
				diff = render.Clean(capture.buffer.String())
				if capture.truncated {
					note = prompts.ToolDiffTruncated
				}
			}
		}
		if diff == "" && note == "" {
			note = prompts.ToolDiffNoChanges
			if !change.Before.Exists && change.After.Exists {
				note = prompts.ToolDiffCreatedEmpty
			} else if change.Before.Exists && !change.After.Exists {
				note = prompts.ToolDiffDeletedEmpty
			}
		}
		if diff != "" {
			detail.WriteString(render.Fence(diff, "diff") + "\n")
		}
		if note != "" {
			detail.WriteString(render.EscapeInline(note) + "\n\n")
		}
		if len(preview) < 6 {
			preview = append(preview, shortDiffLine(path))
			for _, line := range strings.Split(diff, "\n") {
				if len(preview) == 6 {
					break
				}
				if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
					preview = append(preview, shortDiffLine(line))
				}
			}
			if note != "" && len(preview) < 6 {
				preview = append(preview, shortDiffLine(note))
			}
		}
	}
	result := fileResult{value: value, detail: detail.String()}
	if len(preview) != 0 {
		result.summary = "\n\n" + render.Fence(strings.Join(preview, "\n"), "diff")
	}
	return result
}

func shortDiffLine(text string) string {
	runes := []rune(render.Clean(text))
	if len(runes) > 120 {
		return string(runes[:120]) + "…"
	}
	return strings.ReplaceAll(strings.ReplaceAll(string(runes), "\n", " "), "\t", " ")
}
