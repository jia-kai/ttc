package jobs

import (
	"sort"
	"strings"
	"unicode"
)

// ReferenceLabel returns a bounded launch identity, preferring an explicit label,
// then a normalized command preview, then the exact job ID. Unlike Label, it
// excludes generic display defaults so unlabeled jobs remain distinguishable.
func (s Snapshot) ReferenceLabel() string {
	if s.referenceLabel != "" {
		return s.referenceLabel
	}
	return s.ID
}

// Metadata is the output-free projection used by routine job lists and runtime
// snapshots. Command is included only by an explicitly detailed job list.
type Metadata struct {
	ID                    string      `json:"job_id"`
	Kind                  string      `json:"kind"`
	Owner                 string      `json:"owner_actor_id"`
	Label                 string      `json:"label"`
	RecordDir             string      `json:"record_dir,omitempty"`
	Status                string      `json:"status"`
	StartedAt             string      `json:"started_at"`
	FinishedAt            string      `json:"finished_at,omitempty"`
	ExitCode              *int        `json:"exit_code,omitempty"`
	Signal                string      `json:"signal,omitempty"`
	Background            bool        `json:"background,omitempty"`
	Promoted              bool        `json:"promoted,omitempty"`
	TimeoutSeconds        *float64    `json:"timeout_seconds,omitempty"`
	ForegroundWaitSeconds *float64    `json:"max_fg_wait_seconds,omitempty"`
	CancellationReason    string      `json:"cancellation_reason,omitempty"`
	Command               string      `json:"command,omitempty"`
	Grep                  *GrepOrigin `json:"grep_origin,omitempty"`
}

// Metadata returns an independent, output-free snapshot without the command.
func (s Snapshot) Metadata() Metadata {
	v := Metadata{ID: s.ID, Kind: s.Kind, Owner: s.Owner, Label: s.Label, RecordDir: s.RecordDir, Status: s.Status, StartedAt: s.StartedAt, FinishedAt: s.FinishedAt, Signal: s.Signal}
	v.Background, v.Promoted, v.CancellationReason = s.Background, s.Promoted, s.CancellationReason
	if s.TimeoutSeconds != nil {
		seconds := *s.TimeoutSeconds
		v.TimeoutSeconds = &seconds
	}
	if s.ForegroundWaitSeconds != nil {
		seconds := *s.ForegroundWaitSeconds
		v.ForegroundWaitSeconds = &seconds
	}
	if s.Grep != nil {
		origin := *s.Grep
		v.Grep = &origin
	}
	if s.ExitCode != nil {
		code := *s.ExitCode
		v.ExitCode = &code
	}
	return v
}

// compactLabel replaces controls with spaces, normalizes whitespace, and bounds
// labels to 80 Unicode code points so terminal display cannot execute escapes.
func compactLabel(label, fallback string) string {
	label = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, label)
	label = strings.Join(strings.Fields(label), " ")
	if label == "" {
		label = fallback
	}
	runes := []rune(label)
	if len(runes) > 80 {
		label = string(runes[:79]) + "…"
	}
	return label
}

// ListMetadata returns sorted, owner-scoped job metadata without reading capture
// buffers. details includes exact commands, never captured output.
func (m *Manager) ListMetadata(owner string, all, details bool) []Metadata {
	m.mu.Lock()
	out := []Metadata{}
	for _, j := range m.jobs {
		if allowed(owner, j.view.Owner) && (all || j.view.Status == "running") {
			v := j.view.Metadata()
			if details {
				v.Command = j.view.command
			}
			out = append(out, v)
		}
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
