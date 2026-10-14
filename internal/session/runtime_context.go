package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/scratch"
	"ttc/internal/skills"
	"ttc/internal/tool"
	"ttc/internal/workspace"
)

// contextCursor belongs to one actor, advances after successful request admission,
// and survives compaction. Explicit session changes discard it with live state.
type contextCursor struct {
	jobs, timers map[string]string
	project      string            // Exact JSON of the last supplied project context.
	snapshot     string            // Stable runtime state, excluding one-shot changes and project updates.
	capabilities string            // Compact native-read announcement in the last admitted context.
	git          workspace.GitInfo // Git metadata sampled on this actor's first request.
}
type projectInstruction struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}
type projectContext struct {
	Instructions []projectInstruction `json:"instructions"`
	Skills       []skills.Skill       `json:"available_skills"`
}

// projectInheritedSkillCatalogs filters historical announcements for their new
// receiver. Only catalog entries change: actor provenance, other runtime fields,
// and historical instructions/tool evidence remain intact. The parent slice and
// saved messages are never modified. A fresh runtime context announces the full
// receiving catalog separately.
func (r *Runtime) projectInheritedSkillCatalogs(messages []llm.Message, actor string) ([]llm.Message, error) {
	available, err := r.Skills.List(actor)
	if err != nil {
		return nil, err
	}
	role, err := skills.RoleForActor(actor)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(available))
	for _, skill := range available {
		allowed[skill.Name] = true
	}
	projected := append([]llm.Message(nil), messages...)
	for i, message := range projected {
		if !message.Runtime || message.Role != "developer" {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(message.Content), &fields) != nil || string(fields["type"]) != `"runtime_context"` {
			continue
		}
		var project map[string]json.RawMessage
		if err := json.Unmarshal(fields["project"], &project); err != nil {
			if len(fields["project"]) == 0 {
				continue
			}
			return nil, err
		}
		catalog, exists := project["available_skills"]
		if !exists {
			continue
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(catalog, &entries); err != nil {
			return nil, err
		}
		filtered := make([]json.RawMessage, 0, len(entries))
		for _, entry := range entries {
			var skill skills.Skill
			if err := json.Unmarshal(entry, &skill); err != nil {
				return nil, err
			}
			// Current discovery also excludes restricted legacy announcements that
			// predate roles metadata. Historical roles can only narrow visibility.
			if allowed[skill.Name] && (len(skill.Roles) == 0 || slices.Contains(skill.Roles, role)) {
				filtered = append(filtered, entry)
			}
		}
		if len(filtered) == len(entries) {
			continue
		}
		project["available_skills"], err = json.Marshal(filtered)
		if err != nil {
			return nil, err
		}
		fields["project"], err = json.Marshal(project)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		projected[i].Content = string(data)
	}
	return projected, nil
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
	Type        string             `json:"type"`
	Actor       string             `json:"actor"`
	Changes     []stateChange      `json:"changes_since_previous_request"`
	Cwd         string             `json:"cwd"`
	IsRepo      bool               `json:"is_repo"`
	Branch      string             `json:"branch,omitempty"` // "detached" when HEAD has no branch.
	Scratch     string             `json:"scratch_directory"`
	Date        string             `json:"date_utc"`
	Model       string             `json:"model"`
	ImageInput  bool               `json:"image_input"`
	BinaryFiles *[]nativeReadGroup `json:"binary_files,omitempty"` // Present only on initial/compacted admission or capability changes; [] clears support.
	ImageClick  bool               `json:"image_click"`
	Children    []tool.ChildView   `json:"children"`
	Jobs        []jobs.Snapshot    `json:"live_jobs"`
	Timers      []TimerView        `json:"live_timers"`
	Project     *projectContext    `json:"project,omitempty"` // Absent when unchanged; empty lists explicitly clear it.
}

// nativeReadGroup describes extensions sharing a conservative original-byte
// limit. MIME identifiers and validation remain in the tool/provider catalog.
type nativeReadGroup struct {
	Extensions []string `json:"extensions"`
	MaxBytes   int      `json:"max_bytes"`
}

func nativeReadGroups(formats []llm.BinaryFileType) []nativeReadGroup {
	limits := map[string]int{}
	for _, format := range formats {
		for _, extension := range format.Extensions {
			// An overlapping extension can select more than one MIME entry. Use
			// the smallest limit so the announcement never overpromises support.
			if limit, exists := limits[extension]; !exists || format.MaxBytes < limit {
				limits[extension] = format.MaxBytes
			}
		}
	}
	byLimit := map[int][]string{}
	for extension, limit := range limits {
		byLimit[limit] = append(byLimit[limit], extension)
	}
	groups := make([]nativeReadGroup, 0, len(byLimit))
	for limit, extensions := range byLimit {
		sort.Strings(extensions)
		groups = append(groups, nativeReadGroup{Extensions: extensions, MaxBytes: limit})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].MaxBytes < groups[j].MaxBytes })
	return groups
}

// runtimeContext returns nil when this actor's last admitted state is unchanged.
// The returned cursor is committed only after successful request admission.
func (r *Runtime) runtimeContext(ctx context.Context, actor string, selection llm.Selection, previous contextCursor) (*llm.Message, contextCursor, error) {
	r.orderMu.Lock()
	defer r.orderMu.Unlock()
	return r.runtimeContextLocked(ctx, actor, selection, previous)
}
func (r *Runtime) runtimeContextLocked(ctx context.Context, actor string, selection llm.Selection, previous contextCursor) (*llm.Message, contextCursor, error) {
	path, err := scratch.Verify()
	if err != nil {
		return nil, previous, err
	}
	available, err := r.Skills.List(actor)
	if err != nil {
		return nil, previous, err
	}
	project := projectContext{Instructions: []projectInstruction{}, Skills: available}
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
			return nil, previous, fmt.Errorf(prompts.SessionReadProjectInstructions, p, err)
		}
	}
	projectJSON, err := json.Marshal(project)
	if err != nil {
		return nil, previous, err
	}
	next := contextCursor{jobs: map[string]string{}, timers: map[string]string{}, project: string(projectJSON)}
	next.git = previous.git
	if previous.snapshot == "" {
		next.git = workspace.InspectGit(ctx, r.Workspace.Root)
		if err := ctx.Err(); err != nil {
			return nil, previous, err
		}
	}
	r.images.mu.Lock()
	clicks := r.images.enabled
	r.images.mu.Unlock()
	v := runtimeContext{Type: "runtime_context", Actor: actor, Changes: []stateChange{}, Cwd: r.Workspace.Root, Scratch: path, Date: time.Now().UTC().Format("2006-01-02"), Model: selection.Provider + "/" + selection.Model.ID + "/" + selection.Variant, ImageInput: selection.Model.Images, ImageClick: clicks, Children: r.committedChildren(actor), Jobs: []jobs.Snapshot{}, Timers: []TimerView{}}
	v.IsRepo, v.Branch = next.git.Repo != "", next.git.Branch
	formats := nativeReadGroups(selection.Model.BinaryFileTypes())
	v.BinaryFiles = &formats
	capabilities, err := json.Marshal(formats)
	if err != nil {
		return nil, previous, err
	}
	next.capabilities = string(capabilities)
	if next.project != previous.project {
		v.Project = &project
	}
	for _, j := range r.committedJobs(actor) {
		status := j.Status
		next.jobs[j.ID] = status
		if previous.jobs[j.ID] != status {
			v.Changes = append(v.Changes, stateChange{Kind: "job", ID: j.ID, Label: j.Label, From: previous.jobs[j.ID], To: status, ExitCode: j.ExitCode, Signal: j.Signal, FinishedAt: j.FinishedAt})
		}
		if j.Status == "running" {
			v.Jobs = append(v.Jobs, j)
		}
	}
	for _, timer := range r.committedTimers() {
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
	// Compare persistent state separately from one-shot delivery fields: clearing
	// last request's transitions or project update must not create a new entry.
	stable := v
	stable.Changes, stable.Project = nil, nil
	data, err := json.Marshal(stable)
	if err != nil {
		return nil, previous, err
	}
	next.snapshot = string(data)
	// Stable comparison always includes the effective formats; suppressing the
	// announcement on the wire must not itself produce a follow-up snapshot.
	if previous.snapshot != "" && previous.capabilities == next.capabilities {
		v.BinaryFiles = nil
	}
	if next.snapshot == previous.snapshot && v.Project == nil && len(v.Changes) == 0 {
		return nil, next, nil
	}
	data, err = json.Marshal(v)
	if err != nil {
		return nil, previous, err
	}
	return &llm.Message{Role: "developer", Runtime: true, Content: string(data)}, next, nil
}

func contextLabel(m llm.Message) string {
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
