package tool

import (
	"context"
	"errors"
	"time"
	"ttc/internal/capture"

	"ttc/internal/jobs"
	"ttc/internal/prompts"
	"ttc/internal/workspace"
)

type shellArgs struct {
	Label          string `json:"label,omitempty"`
	Command        string `json:"command"`
	Workdir        string `json:"workdir,omitempty"`
	Timeout        *int   `json:"timeout_seconds,omitempty"`
	ForegroundWait *int   `json:"max_fg_wait_seconds,omitempty"`
	Background     bool   `json:"background,omitempty"`
	Protocol       string `json:"protocol,omitempty"`
	Strict         *bool  `json:"strict,omitempty"` // Nil enables set -eu; false disables it.
	OutputMode     string `json:"output_mode,omitempty"`
	OutputBytes    *int   `json:"output_bytes,omitempty"`
	StripANSI      *bool  `json:"strip_ansi,omitempty"`
}

// ChildView is copied live metadata for one reusable coding context.
type ChildView struct {
	ID               string `json:"child_id"`
	Label            string `json:"label"`
	ChildState       string `json:"child_state"`       // Context lifecycle: running, idle or closed at observation.
	AssignmentStatus string `json:"assignment_status"` // Current or latest assignment: running, completed, failed or cancelled.
	TurnID           string `json:"child_turn_id"`
	JobID            string `json:"job_id"`
}

// ChildController supplies optional coding-child ownership to job tools.
type ChildController interface {
	ChildViews(string) []ChildView
	StopChild(context.Context, string, string) (any, error)
}

// AddShell exposes managed commands and owner-scoped live handles. A nil child
// controller leaves standalone shell registries without reusable child contexts.
func AddShell(r *Registry, m *jobs.Manager, w *workspace.Manager, children ChildController) {
	Register(r, "shell", prompts.ToolDescription("shell"), map[string]any{"command": Property("string"), "label": Property("string"), "workdir": Property("string"), "timeout_seconds": Property("integer"), "max_fg_wait_seconds": Property("integer"), "background": Property("boolean"), "protocol": Property("string", "lsp"), "strict": Property("boolean"), "output_mode": Property("string", "head", "tail", "head_tail"), "output_bytes": IntegerRange(1, capture.MaxOutputBytes), "strip_ansi": Property("boolean")}, []string{"command"}, func(a shellArgs) error {
		if e := Required("command", a.Command); e != nil {
			return e
		}
		if a.Protocol != "" && a.Protocol != "lsp" {
			return errors.New(prompts.ToolShellProtocol)
		}
		_, e := (capture.OutputOptions{Mode: a.OutputMode}).Normalize()
		return e
	}, func(ctx context.Context, x Execution, a shellArgs) (any, error) {
		dir := w.Root
		if a.Workdir != "" {
			dir = w.Path(a.Workdir)
		}
		timeout := jobs.ExecutionTimeout
		if a.Protocol == "lsp" {
			timeout = 0
		}
		if a.Timeout != nil {
			timeout = time.Duration(*a.Timeout) * time.Second
		}
		wait := time.Duration(intDefault(a.ForegroundWait, 10)) * time.Second
		var id string
		var e error
		output := capture.OutputOptions{Mode: a.OutputMode, Bytes: intDefault(a.OutputBytes, capture.DefaultOutputBytes), StripANSI: a.StripANSI}
		if a.Protocol == "lsp" {
			id, e = m.StartLSP(x.Actor, a.Command, dir, timeout, a.Strict == nil || *a.Strict, output, a.Label)
		} else {
			id, e = m.Start(x.Actor, a.Command, dir, timeout, a.Strict == nil || *a.Strict, a.Background, output, a.Label)
		}
		if e != nil {
			return nil, jobToolError(e)
		}
		if a.Background {
			v, err := m.View(x.Actor, id)
			return v, jobToolError(err)
		}
		v, err := m.WaitForeground(ctx, x.Actor, id, wait, func(v jobs.Snapshot) {
			if x.Update != nil {
				x.Update(v)
			}
		})
		return v, jobToolError(err)
	}, func(a *shellArgs) []InputWarning {
		warnings := NormalizeEnum("protocol", &a.Protocol, "lsp")
		warnings = append(warnings, NormalizeEnum("output_mode", &a.OutputMode, "head", "tail", "head_tail")...)
		if a.Protocol == "lsp" && !a.Background {
			warnings = append(warnings, AdjustedInput("background", false, true)...)
			a.Background = true
		}
		if a.Background && a.ForegroundWait != nil {
			warnings = append(warnings, AdjustedInput("max_fg_wait_seconds", *a.ForegroundWait, nil)...)
			a.ForegroundWait = nil
		} else if a.ForegroundWait != nil {
			warnings = append(warnings, ClipInteger("max_fg_wait_seconds", a.ForegroundWait, 0, 86400)...)
		}
		warnings = append(warnings, NormalizeTimeoutSeconds(a.Timeout, 86400)...)
		return warnings
	})
	type list struct {
		State   string `json:"state,omitempty"`
		Details bool   `json:"details,omitempty"`
	}
	Register(r, "job_list", prompts.ToolDescription("job_list"), map[string]any{"state": Property("string", "running", "all"), "details": Property("boolean")}, nil, func(a list) error {
		if a.State != "" && a.State != "running" && a.State != "all" {
			return errors.New(prompts.ToolJobListState)
		}
		return nil
	}, func(ctx context.Context, x Execution, a list) (any, error) {
		result := map[string]any{"jobs": m.ListMetadata(x.Actor, a.State == "all", a.Details)}
		if children != nil {
			views := []ChildView{}
			for _, child := range children.ChildViews(x.Actor) {
				if a.State == "all" || child.ChildState == "running" {
					views = append(views, child)
				}
			}
			result["children"] = views
		}
		return result, nil
	}, func(a *list) []InputWarning { return NormalizeEnum("state", &a.State, "running", "all") })
	type read struct {
		ID            string `json:"job_id"`
		Cursor        string `json:"cursor,omitempty"`
		Stream        string `json:"stream,omitempty"`
		Grep          string `json:"grep,omitempty"`
		Literal       bool   `json:"literal,omitempty"`
		CaseSensitive *bool  `json:"case_sensitive,omitempty"`
		Limit         *int   `json:"limit_bytes,omitempty"`
		StripANSI     *bool  `json:"strip_ansi,omitempty"`
	}
	Register(r, "job_read", prompts.ToolDescription("job_read"), map[string]any{"job_id": Property("string"), "cursor": Property("string"), "stream": Property("string", "stdout", "stderr"), "grep": Property("string"), "literal": Property("boolean"), "case_sensitive": Property("boolean"), "limit_bytes": IntegerRange(1, 65536), "strip_ansi": Property("boolean")}, []string{"job_id"}, func(a read) error {
		if e := Required("job_id", a.ID); e != nil {
			return e
		}
		return nil
	}, func(ctx context.Context, x Execution, a read) (any, error) {
		v, e := m.Read(ctx, x.Actor, a.ID, jobs.ReadOptions{Stream: a.Stream, Cursor: a.Cursor, Limit: intDefault(a.Limit, 16384), Grep: a.Grep, Literal: a.Literal, IgnoreCase: a.CaseSensitive != nil && !*a.CaseSensitive, StripANSI: a.StripANSI})
		if e != nil {
			return nil, jobToolError(e)
		}
		return v, nil
	}, func(a *read) []InputWarning { return NormalizeEnum("stream", &a.Stream, "stdout", "stderr") })
	type stop struct {
		ID    string `json:"job_id,omitempty"`
		Child string `json:"child_id,omitempty"`
	}
	Register(r, "job_stop", prompts.ToolDescription("job_stop"), map[string]any{"job_id": Property("string"), "child_id": Property("string")}, nil, func(a stop) error {
		if (a.ID == "") == (a.Child == "") {
			return Fail("invalid_arguments", prompts.ToolJobStopIdentifier)
		}
		return nil
	}, func(ctx context.Context, x Execution, a stop) (any, error) {
		if a.Child != "" {
			if children == nil {
				return nil, Fail("not_found", prompts.ToolNoCodingChildren)
			}
			return children.StopChild(ctx, x.Actor, a.Child)
		}
		v, e := m.Stop(x.Actor, a.ID)
		if e != nil {
			return nil, Fail("not_found", e.Error())
		}
		return map[string]any{"job_id": v.ID, "kind": v.Kind, "label": v.ReferenceLabel(), "status": v.Status}, nil
	})
	addLSP(r, m, w)
}
