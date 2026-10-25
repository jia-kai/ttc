package tool

import (
	"context"
	"errors"
	"strings"
	"time"
	"ttc/internal/capture"
	"ttc/internal/jobs"
	"ttc/internal/prompts"
	"ttc/internal/watchrun"
	"ttc/internal/workspace"
	"unicode"
	"unicode/utf8"
)

type watchArgs struct {
	RecordDir string `json:"record_dir"`
	Label     string `json:"label,omitempty"`
	StripANSI *bool  `json:"strip_ansi,omitempty"`
}

type tmuxWatchArgs struct {
	Command   string `json:"command"`
	Label     string `json:"label,omitempty"`
	RecordDir string `json:"record_dir"`
	Workdir   string `json:"workdir,omitempty"`
	StripANSI *bool  `json:"strip_ansi,omitempty"`
}

// normalizeWatchLabel bounds cosmetic text, never command or record identity.
// Both watch tools use the same UTF-8 byte budget as the tmux window label.
func normalizeWatchLabel(label *string) []InputWarning {
	original := *label
	value := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, original)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 64 {
		value = value[:64]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
		value = strings.TrimSpace(value)
	}
	*label = value
	if original == value {
		return nil
	}
	return AdjustedInput("label", original, value)
}

// AddTmuxWatchRun registers durable detached launches with runtime-local observation.
func AddTmuxWatchRun(r *Registry, m *jobs.Manager, w *workspace.Manager) {
	addTmuxWatchRun(r, m, w, watchrun.TmuxLauncher{})
}

func addTmuxWatchRun(r *Registry, m *jobs.Manager, w *workspace.Manager, launcher watchrun.TmuxLauncher) {
	Register(r, "tmux_watch_run", prompts.ToolDescription("tmux_watch_run"), map[string]any{"command": Property("string"), "label": Property("string"), "record_dir": Property("string"), "workdir": Property("string"), "strip_ansi": Property("boolean")}, []string{"command", "record_dir"}, func(a tmuxWatchArgs) error {
		if err := Required("command", a.Command); err != nil {
			return err
		}
		if err := Required("record_dir", a.RecordDir); err != nil {
			return err
		}
		return nil
	}, func(ctx context.Context, x Execution, a tmuxWatchArgs) (any, error) {
		dir := w.Root
		if a.Workdir != "" {
			dir = w.Path(a.Workdir)
		}
		launch, err := launcher.Launch(ctx, a.Command, a.Label, w.Path(a.RecordDir), dir)
		result := map[string]any{"session_name": launch.SessionName, "record_dir": launch.RecordDir}
		if launch.SessionID != "" {
			result["session_id"] = launch.SessionID
		}
		if launch.PaneID != "" {
			result["pane_id"] = launch.PaneID
		}
		if err != nil {
			result["status"] = "launch_failed"
			if launch.Started {
				result["status"] = "launch_unknown"
			}
			result["error"] = err.Error()
			return result, nil
		}
		result["status"] = "launched_unobserved"
		ready, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err = watchrun.WaitReady(ready, launch.RecordDir); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				result["error"] = prompts.TmuxWatchReadinessTimeout
			} else {
				result["error"] = err.Error()
			}
			return result, nil
		}
		id, err := m.StartWatch(x.Actor, launch.RecordDir, a.Label, capture.OutputOptions{StripANSI: a.StripANSI})
		if err != nil {
			result["error"] = err.Error()
			return result, nil
		}
		result["job_id"] = id
		view, err := m.View(x.Actor, id)
		if err != nil {
			result["error"] = err.Error()
			return result, nil
		}
		result["status"] = "watching"
		result["watcher"] = view
		return result, nil
	}, func(a *tmuxWatchArgs) []InputWarning { return normalizeWatchLabel(&a.Label) })
}

// AddWatch registers observation of externally owned watch-run records. The
// resulting job supports job_read/list/stop; stopping it never kills workload.
func AddWatch(r *Registry, m *jobs.Manager, w *workspace.Manager) {
	Register(r, "watch", prompts.ToolDescription("watch"), map[string]any{"record_dir": Property("string"), "label": Property("string"), "strip_ansi": Property("boolean")}, []string{"record_dir"}, func(a watchArgs) error { return Required("record_dir", a.RecordDir) }, func(ctx context.Context, x Execution, a watchArgs) (any, error) {
		id, err := m.StartWatch(x.Actor, w.Path(a.RecordDir), a.Label, capture.OutputOptions{StripANSI: a.StripANSI})
		if err != nil {
			return nil, jobToolError(err)
		}
		v, err := m.View(x.Actor, id)
		return v, jobToolError(err)
	}, func(a *watchArgs) []InputWarning { return normalizeWatchLabel(&a.Label) })
}
