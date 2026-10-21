package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
// and is reset for fresh/compacted contexts. It never restores executable work.
type contextCursor struct {
	snapshot *runtimeContext   // Immutable state represented by the last admitted baseline/update.
	git      workspace.GitInfo // Git metadata sampled on this actor's first request.
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
		if message.Type != llm.TypeRuntimeContext {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(message.Content), &fields) != nil {
			continue
		}
		container := fields
		switch string(fields["type"]) {
		case `"runtime_context"`:
		case `"runtime_context_update"`:
			container = nil
			if err := json.Unmarshal(fields["set"], &container); err != nil {
				return nil, err
			}
		default:
			continue
		}
		var project map[string]json.RawMessage
		if err := json.Unmarshal(container["project"], &project); err != nil {
			if len(container["project"]) == 0 {
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
		container["project"], err = json.Marshal(project)
		if err != nil {
			return nil, err
		}
		if string(fields["type"]) == `"runtime_context_update"` {
			fields["set"], err = json.Marshal(container)
			if err != nil {
				return nil, err
			}
		}
		data, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		projected[i].Content = string(data)
	}
	return projected, nil
}

// runtimeContext is a frozen view, not a second mutable source of runtime state.
// Activity inventories are baseline-only bootstrap data; tool results and actor
// events carry subsequent activity without duplicating it in context updates.
type runtimeContext struct {
	Type        string             `json:"type"`
	Actor       string             `json:"actor"`
	Cwd         string             `json:"cwd"`
	IsRepo      bool               `json:"is_repo"`
	Branch      string             `json:"branch"` // Empty outside Git; "detached" when HEAD has no branch.
	Scratch     string             `json:"scratch_directory"`
	Date        string             `json:"date_utc"`
	Model       string             `json:"model"`
	ImageInput  bool               `json:"image_input"`
	BinaryFiles *[]nativeReadGroup `json:"binary_files"`
	ImageClick  bool               `json:"image_click"`
	Children    []tool.ChildView   `json:"children"`
	Jobs        []jobs.Metadata    `json:"live_jobs"`
	Timers      []TimerView        `json:"live_timers"`
	Project     *projectContext    `json:"project"`
}

// runtimeContextUpdate replaces only supplied fields. Project is a namespace:
// its instructions and available_skills lists are replaced independently.
type runtimeContextUpdate struct {
	Type string         `json:"type"`
	Set  map[string]any `json:"set"`
}

func contextUpdate(previous, current *runtimeContext) runtimeContextUpdate {
	update := runtimeContextUpdate{Type: "runtime_context_update", Set: map[string]any{}}
	set := func(name string, before, after any) {
		if !reflect.DeepEqual(before, after) {
			update.Set[name] = after
		}
	}
	set("cwd", previous.Cwd, current.Cwd)
	set("is_repo", previous.IsRepo, current.IsRepo)
	set("branch", previous.Branch, current.Branch)
	set("scratch_directory", previous.Scratch, current.Scratch)
	set("date_utc", previous.Date, current.Date)
	set("model", previous.Model, current.Model)
	set("image_input", previous.ImageInput, current.ImageInput)
	set("image_click", previous.ImageClick, current.ImageClick)
	set("binary_files", previous.BinaryFiles, current.BinaryFiles)
	project := map[string]any{}
	if !reflect.DeepEqual(previous.Project.Instructions, current.Project.Instructions) {
		project["instructions"] = current.Project.Instructions
	}
	if !reflect.DeepEqual(previous.Project.Skills, current.Project.Skills) {
		project["available_skills"] = current.Project.Skills
	}
	if len(project) != 0 {
		update.Set["project"] = project
	}
	return update
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
	next := contextCursor{git: previous.git}
	if previous.snapshot == nil {
		next.git = workspace.InspectGit(ctx, r.Workspace.Root)
		if err := ctx.Err(); err != nil {
			return nil, previous, err
		}
	}
	r.images.mu.Lock()
	clicks := r.images.enabled
	r.images.mu.Unlock()
	v := runtimeContext{Type: "runtime_context", Actor: actor, Cwd: r.Workspace.Root, Scratch: path, Date: time.Now().UTC().Format("2006-01-02"), Model: selection.Provider + "/" + selection.Model.ID + "/" + selection.Variant, ImageInput: selection.Model.Images, ImageClick: clicks, Project: &project, Children: []tool.ChildView{}, Jobs: []jobs.Metadata{}, Timers: []TimerView{}}
	v.IsRepo, v.Branch = next.git.Repo != "", next.git.Branch
	formats := nativeReadGroups(selection.Model.BinaryFileTypes())
	v.BinaryFiles = &formats
	next.snapshot = &v
	var payload any
	if previous.snapshot == nil || previous.snapshot.Actor != actor {
		v.Children = r.committedChildren(actor)
		for _, job := range r.committedJobs(actor) {
			if job.Status == "running" {
				v.Jobs = append(v.Jobs, job.Metadata())
			}
		}
		for _, timer := range r.committedTimers() {
			if timer.Status == "scheduled" && (timer.actor == actor || strings.HasPrefix(timer.actor, actor+"/")) {
				v.Timers = append(v.Timers, TimerView{timer.ID, timer.Name, timer.NextAt, timer.Repeat})
			}
		}
		payload = v
	} else {
		update := contextUpdate(previous.snapshot, &v)
		if len(update.Set) == 0 {
			return nil, next, nil
		}
		payload = update
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, previous, err
	}
	return &llm.Message{Type: llm.TypeRuntimeContext, Content: string(data)}, next, nil
}
