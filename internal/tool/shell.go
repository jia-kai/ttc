package tool

import (
	"context"
	"errors"
	"scicode/internal/jobs"
	"scicode/internal/workspace"
	"time"
)

type shellArgs struct {
	Command    string `json:"command"`
	Workdir    string `json:"workdir,omitempty"`
	Timeout    *int   `json:"timeout_ms,omitempty"`
	Background bool   `json:"background,omitempty"`
	Wake       *bool  `json:"wake_on_exit,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	Strict     *bool  `json:"strict,omitempty"` // Nil enables set -eu; false disables it.
}

// AddShell exposes managed commands and owner-scoped live handles.
func AddShell(r *Registry, m *jobs.Manager, w *workspace.Manager) {
	Register(r, "shell", "Run /bin/sh with set -eu and closed stdin. Set strict=false to disable errexit/nounset; pipefail is not enabled. Use tmux for work that must outlive this runtime.", map[string]any{"command": Property("string"), "workdir": Property("string"), "timeout_ms": Property("integer"), "background": Property("boolean"), "wake_on_exit": Property("boolean"), "protocol": Property("string", "lsp"), "strict": Property("boolean")}, []string{"command"}, func(a shellArgs) error {
		if e := Required("command", a.Command); e != nil {
			return e
		}
		if a.Timeout != nil && (*a.Timeout < 0 || *a.Timeout > 86400000) {
			return errors.New("timeout_ms must be 0–86400000")
		}
		if a.Protocol != "" && a.Protocol != "lsp" {
			return errors.New("unknown protocol")
		}
		if a.Protocol == "lsp" && !a.Background {
			return errors.New("lsp requires background")
		}
		return nil
	}, func(ctx context.Context, x Execution, a shellArgs) (any, error) {
		if a.Protocol != "" {
			return nil, Fail("unsupported_operation", "LSP protocol jobs are not implemented; omit protocol to run an ordinary shell command")
		}
		dir := w.Root
		if a.Workdir != "" {
			dir = w.Path(a.Workdir)
		}
		ms := 120000
		if a.Background {
			ms = 0
		}
		if a.Timeout != nil {
			ms = *a.Timeout
		}
		wake := a.Background && (a.Wake == nil || *a.Wake)
		id, e := m.Start(x.Actor, a.Command, dir, time.Duration(ms)*time.Millisecond, a.Strict == nil || *a.Strict, a.Background, wake)
		if e != nil {
			return nil, e
		}
		if a.Background {
			return m.View(x.Actor, id)
		}
		return m.Wait(ctx, x.Actor, id, func(v jobs.Snapshot) {
			if x.Update != nil {
				x.Update(v)
			}
		})
	})
	type list struct {
		State string `json:"state,omitempty"`
	}
	Register(r, "job_list", "List this live runtime's jobs; old historical IDs cannot be revived.", map[string]any{"state": Property("string", "running", "all")}, nil, func(a list) error {
		if a.State != "" && a.State != "running" && a.State != "all" {
			return errors.New("state must be running or all; omit it to list running jobs")
		}
		return nil
	}, func(ctx context.Context, x Execution, a list) (any, error) {
		return map[string]any{"jobs": m.List(x.Actor, a.State == "all")}, nil
	})
	type read struct {
		ID            string `json:"job_id"`
		Cursor        string `json:"cursor,omitempty"`
		Stream        string `json:"stream,omitempty"`
		Grep          string `json:"grep,omitempty"`
		Literal       bool   `json:"literal,omitempty"`
		CaseSensitive *bool  `json:"case_sensitive,omitempty"`
		Limit         *int   `json:"limit_bytes,omitempty"`
	}
	Register(r, "job_read", "Read stdout or stderr; cursor is an absolute byte offset or eof:-N:bytes / eof:-N:lines. Optional grep filters lines within the page; next_cursor advances over the full page.", map[string]any{"job_id": Property("string"), "cursor": Property("string"), "stream": Property("string", "stdout", "stderr"), "grep": Property("string"), "literal": Property("boolean"), "case_sensitive": Property("boolean"), "limit_bytes": Property("integer")}, []string{"job_id"}, func(a read) error {
		if e := Required("job_id", a.ID); e != nil {
			return e
		}
		return rangeInt("limit_bytes", a.Limit, 1, 65536)
	}, func(ctx context.Context, x Execution, a read) (any, error) {
		v, e := m.Read(ctx, x.Actor, a.ID, jobs.ReadOptions{Stream: a.Stream, Cursor: a.Cursor, Limit: intDefault(a.Limit, 16384), Grep: a.Grep, Literal: a.Literal, IgnoreCase: a.CaseSensitive != nil && !*a.CaseSensitive})
		if e != nil {
			return nil, e
		}
		return v, nil
	})
	type stop struct {
		ID string `json:"job_id"`
	}
	Register(r, "job_stop", "Stop a live command process group.", map[string]any{"job_id": Property("string")}, []string{"job_id"}, func(a stop) error { return Required("job_id", a.ID) }, func(ctx context.Context, x Execution, a stop) (any, error) {
		v, e := m.Stop(x.Actor, a.ID)
		if e != nil {
			return nil, Fail("not_found", e.Error())
		}
		return map[string]any{"job_id": v.ID, "status": v.Status}, nil
	})
}
