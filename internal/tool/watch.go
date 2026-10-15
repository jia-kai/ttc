package tool

import (
	"context"
	"errors"
	"strings"
	"time"
	"ttc/internal/jobs"
	"ttc/internal/prompts"
	"ttc/internal/watchrun"
	"ttc/internal/workspace"
	"unicode"
)

type watchArgs struct {
	RecordDir string `json:"record_dir"`
	Label     string `json:"label,omitempty"`
	Wake      *bool  `json:"wake_on_exit,omitempty"`
}

type tmuxWatchArgs struct {
	Command   string `json:"command"`
	Label     string `json:"label,omitempty"`
	RecordDir string `json:"record_dir"`
	Workdir   string `json:"workdir,omitempty"`
	Wake      *bool  `json:"wake_on_exit,omitempty"`
}

// AddTmuxWatchRun registers durable detached launches with runtime-local observation.
func AddTmuxWatchRun(r *Registry, m *jobs.Manager, w *workspace.Manager) {
	addTmuxWatchRun(r, m, w, watchrun.TmuxLauncher{})
}

func addTmuxWatchRun(r *Registry, m *jobs.Manager, w *workspace.Manager, launcher watchrun.TmuxLauncher) {
	Register(r, "tmux_watch_run", prompts.ToolDescription("tmux_watch_run"), map[string]any{"command": Property("string"), "label": Property("string"), "record_dir": Property("string"), "workdir": Property("string"), "wake_on_exit": Property("boolean")}, []string{"command", "record_dir"}, func(a tmuxWatchArgs) error {
		if err := Required("command", a.Command); err != nil {
			return err
		}
		if err := Required("record_dir", a.RecordDir); err != nil {
			return err
		}
		if len(a.Label) > 64 || strings.ContainsFunc(a.Label, unicode.IsControl) {
			return errors.New(prompts.TmuxWatchInvalidLabel)
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
		id, err := m.StartWatch(x.Actor, launch.RecordDir, a.Wake == nil || *a.Wake, a.Label)
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
	})
}

// AddWatch registers observation of externally owned watch-run records. The
// resulting job supports job_read/list/stop; stopping it never kills workload.
func AddWatch(r *Registry, m *jobs.Manager, w *workspace.Manager) {
	Register(r, "watch", prompts.ToolDescription("watch"), map[string]any{"record_dir": Property("string"), "label": Property("string"), "wake_on_exit": Property("boolean")}, []string{"record_dir"}, func(a watchArgs) error { return Required("record_dir", a.RecordDir) }, func(ctx context.Context, x Execution, a watchArgs) (any, error) {
		id, err := m.StartWatch(x.Actor, w.Path(a.RecordDir), a.Wake == nil || *a.Wake, a.Label)
		if err != nil {
			return nil, jobToolError(err)
		}
		v, err := m.View(x.Actor, id)
		return v, jobToolError(err)
	})
}
