package jobs

import (
	"context"
	"fmt"

	"ttc/internal/prompts"
)

// InspectionField is job-specific display metadata. Language optionally selects
// a fenced-code language; an empty language leaves presentation to the renderer.
type InspectionField struct {
	Name, Value, Language string
}

// InspectionStream requests a retained stream tail. Name must be stdout or
// stderr; LimitBytes is a positive source-byte bound, at most 65536. Markdown
// permits the renderer to present Text as Markdown rather than literal output.
type InspectionStream struct {
	Name, Heading string
	LimitBytes    int
	Markdown      bool
}

// InspectionSpec describes job-specific presentation without reading captures.
// Its fields and stream slices are copied when an inspection is acquired.
type InspectionSpec struct {
	Title   string
	Fields  []InspectionField
	Streams []InspectionStream
}

// Inspector is a cheap, pure presentation callback invoked outside manager
// locks. Install it before launch using TaskOptions. It is never persisted.
type Inspector func(Snapshot) InspectionSpec

// InspectionOutput owns one bounded tail or a deliberate acquisition error.
// Truncated reports any source bytes omitted by retention or the requested bound.
type InspectionOutput struct {
	InspectionStream
	Text      string
	Truncated bool
	Error     string
}

// Inspection owns copied metadata, presentation configuration, and capture tails.
// Job is the metadata snapshot supplied to Inspector, not a subsequent refresh.
type Inspection struct {
	Job    Snapshot
	Spec   InspectionSpec
	Output []InspectionOutput
}

// TaskOptions sets task identity, lifecycle, and optional inspection behavior.
// A nil Inspector selects the label field and 8192-byte stdout/stderr tails.
// Background controls completion notification; WakeOnExit controls model wakeup.
type TaskOptions struct {
	Kind, Label            string
	Background, WakeOnExit bool
	Inspector              Inspector
	// StartWatch retains record_dir in Snapshot for the tool JSON contract.
	recordDir string
	command   string // Exact command retained by watch tasks for explicit detail lists.
}

const inspectionMaxBytes = 65536

func defaultInspectionStreams(protocol bool) []InspectionStream {
	streams := []InspectionStream{{Name: "stdout", Heading: "stdout · showing tail, up to 8 KiB", LimitBytes: 8192}, {Name: "stderr", Heading: "stderr · showing tail, up to 8 KiB", LimitBytes: 8192}}
	if protocol {
		return streams[1:]
	}
	return streams
}

func taskInspector(view Snapshot) InspectionSpec {
	return InspectionSpec{Fields: []InspectionField{{Name: "Label", Value: view.Label}}, Streams: defaultInspectionStreams(view.Kind == "lsp")}
}

func commandInspector(view Snapshot) InspectionSpec {
	return InspectionSpec{Fields: []InspectionField{{Name: "Command", Value: view.command, Language: "sh"}}, Streams: defaultInspectionStreams(view.Kind == "lsp")}
}

// Inspect enforces live-handle ownership and checks cancellation before invoking
// the inspector and before acquisition. The callback receives an independent
// metadata copy outside locks. All valid requested tails are then acquired in one
// manager-lock phase; each buffer read is independently synchronized and writes
// may interleave between tails or occur after the metadata copy.
// Invalid stream names, bounds, and LSP stdout requests produce per-output Error
// values without reading that stream. Captures remain inspectable after completion
// until Close discards the handle. No rendering or history lookup occurs here.
func (m *Manager) Inspect(ctx context.Context, owner, id string) (Inspection, error) {
	if err := ctx.Err(); err != nil {
		return Inspection{}, err
	}
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok || !allowed(owner, j.view.Owner) {
		m.mu.Unlock()
		return Inspection{}, ErrNotFound
	}
	view, inspector := cloneSnapshot(j.view), j.inspector
	protocol := j.client != nil || j.view.Kind == "lsp"
	m.mu.Unlock()
	if inspector == nil {
		inspector = taskInspector
	}
	if err := ctx.Err(); err != nil {
		return Inspection{}, err
	}
	spec := inspector(cloneSnapshot(view))
	spec.Fields = append([]InspectionField(nil), spec.Fields...)
	spec.Streams = append([]InspectionStream(nil), spec.Streams...)
	inspection := Inspection{Job: view, Spec: spec, Output: make([]InspectionOutput, len(spec.Streams))}
	for i, stream := range spec.Streams {
		output := InspectionOutput{InspectionStream: stream}
		switch {
		case protocol && stream.Name == "stdout":
			output.Error = prompts.JobLSPProtocolStdout
		case stream.Name != "stdout" && stream.Name != "stderr":
			output.Error = prompts.JobInvalidStream
		case stream.LimitBytes < 1 || stream.LimitBytes > inspectionMaxBytes:
			output.Error = fmt.Sprintf("Inspection stream limit_bytes must be between 1 and %d.", inspectionMaxBytes)
		}
		inspection.Output[i] = output
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Close may have discarded the handle while the callback ran.
	if m.jobs[id] != j || !allowed(owner, j.view.Owner) {
		return Inspection{}, ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return Inspection{}, err
	}
	for i := range inspection.Output {
		output := &inspection.Output[i]
		if output.Error != "" {
			continue
		}
		buffer := j.stdout
		if output.Name == "stderr" {
			buffer = j.stderr
		}
		page, err := buffer.Read(fmt.Sprintf("eof:-%d:bytes", output.LimitBytes), output.LimitBytes)
		if err != nil {
			output.Error = err.Error()
			continue
		}
		output.Text = page.Output
		// EOF-relative reads mark an overshoot before byte zero as lost, even
		// when all source bytes fit. Only a positive start omits a real prefix.
		output.Truncated = page.Start > 0
	}
	if err := ctx.Err(); err != nil {
		return Inspection{}, err
	}
	return inspection, nil
}

func cloneSnapshot(view Snapshot) Snapshot {
	if view.ExitCode != nil {
		code := *view.ExitCode
		view.ExitCode = &code
	}
	if view.StdoutSelection != nil {
		selection := *view.StdoutSelection
		view.StdoutSelection = &selection
	}
	if view.StderrSelection != nil {
		selection := *view.StderrSelection
		view.StderrSelection = &selection
	}
	return view
}
