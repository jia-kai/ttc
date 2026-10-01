package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"scicode/internal/jobs"
	"scicode/internal/provider"
	"scicode/internal/scratch"
	"scicode/internal/skills"
)

// contextCursor belongs to one actor, advances after a committed context entry,
// and survives compaction. Explicit session changes discard it with live state.
type contextCursor struct {
	jobs, timers map[string]string
	project      string // Exact JSON of the last supplied project context.
}
type projectInstruction struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}
type projectContext struct {
	Instructions []projectInstruction `json:"instructions"`
	Skills       []skills.Skill       `json:"available_skills"`
}
type stateChange struct {
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	Label      string `json:"label"`
	From       string `json:"from"` // Empty when first observed by this actor.
	To         string `json:"to"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Signal     string `json:"signal,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	FiredCount int    `json:"fired_count,omitempty"` // Total timer firings observed; a repeat can remain live after firing.
}
type runtimeContext struct {
	Type       string          `json:"type"`
	Actor      string          `json:"actor"`
	Changes    []stateChange   `json:"changes_since_previous_request"`
	Cwd        string          `json:"working_directory"`
	Scratch    string          `json:"scratch_directory"`
	Date       string          `json:"date_utc"`
	Model      string          `json:"model"`
	ImageInput bool            `json:"image_input"`
	ImageClick bool            `json:"image_click"`
	Jobs       []jobs.Snapshot `json:"live_jobs"`
	Timers     []TimerView     `json:"live_timers"`
	Project    *projectContext `json:"project,omitempty"` // Absent when unchanged; empty lists explicitly clear it.
}

func (r *Runtime) runtimeContext(ctx context.Context, actor string, selection provider.Selection, previous contextCursor) (provider.Message, contextCursor, error) {
	path, err := scratch.Verify()
	if err != nil {
		return provider.Message{}, previous, err
	}
	project := projectContext{Instructions: []projectInstruction{}, Skills: r.Skills.List()}
	var dirs []string
	for d := r.Workspace.Root; ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if d == filepath.Dir(d) {
			break
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		p := filepath.Join(dirs[i], "AGENTS.md")
		b, err := skills.ReadInstruction(ctx, p)
		if err == nil {
			project.Instructions = append(project.Instructions, projectInstruction{Path: p, Content: string(b)})
		} else if !os.IsNotExist(err) {
			return provider.Message{}, previous, fmt.Errorf("read project instructions %s: %w", p, err)
		}
	}
	projectJSON, err := json.Marshal(project)
	if err != nil {
		return provider.Message{}, previous, err
	}
	next := contextCursor{jobs: map[string]string{}, timers: map[string]string{}, project: string(projectJSON)}
	r.images.mu.Lock()
	clicks := r.images.enabled
	r.images.mu.Unlock()
	v := runtimeContext{Type: "runtime_context", Actor: actor, Changes: []stateChange{}, Cwd: r.Workspace.Root, Scratch: path, Date: time.Now().UTC().Format("2006-01-02"), Model: selection.Provider + "/" + selection.Model.ID + "/" + selection.Variant, ImageInput: selection.Model.Images, ImageClick: clicks, Jobs: []jobs.Snapshot{}, Timers: []TimerView{}}
	if next.project != previous.project {
		v.Project = &project
	}
	for _, j := range r.Jobs.Metadata(actor) {
		status := j.Status
		// A shell can exit normally with a nonzero code; expose it as failure in
		// context while preserving the supervisor's exact status and exit code.
		if status == "completed" && (j.ExitCode != nil && *j.ExitCode != 0 || j.Signal != "") {
			status = "failed"
		}
		next.jobs[j.ID] = status
		if previous.jobs[j.ID] != status {
			v.Changes = append(v.Changes, stateChange{Kind: "job", ID: j.ID, Label: j.Label, From: previous.jobs[j.ID], To: status, ExitCode: j.ExitCode, Signal: j.Signal, FinishedAt: j.FinishedAt})
		}
		if j.Status == "running" {
			v.Jobs = append(v.Jobs, j)
		}
	}
	for _, timer := range r.timers.list() {
		state := fmt.Sprintf("%s/%d", timer.Status, timer.Fired)
		next.timers[timer.ID] = state
		if previous.timers[timer.ID] != state {
			from, _, _ := strings.Cut(previous.timers[timer.ID], "/")
			to := timer.Status
			if timer.Fired > 0 && timer.Status == "scheduled" {
				to = "fired"
			}
			v.Changes = append(v.Changes, stateChange{Kind: "timer", ID: timer.ID, Label: timer.Name, From: from, To: to, FiredCount: timer.Fired})
		}
		if timer.Status == "scheduled" {
			v.Timers = append(v.Timers, TimerView{timer.ID, timer.Name, timer.NextAt, timer.Repeat})
		}
	}
	data, err := json.Marshal(v)
	return provider.Message{Role: "developer", Runtime: true, Content: string(data)}, next, err
}

func contextLabel(m provider.Message) string {
	var v runtimeContext
	if json.Unmarshal([]byte(m.Content), &v) != nil {
		return "Runtime context · inspect"
	}
	parts := []string{"Runtime context"}
	for _, status := range []string{"failed", "cancelled", "completed", "running", "fired", "scheduled"} {
		count := 0
		for _, c := range v.Changes {
			if c.To == status {
				count++
			}
		}
		if count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, status))
		}
	}
	return strings.Join(append(parts, "inspect"), " · ")
}
