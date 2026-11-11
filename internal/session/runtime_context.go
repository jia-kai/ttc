package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	easy     *llm.Selection    // Full easy selection sampled with the runtime announcement.
}
type projectInstruction struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}
type projectContext struct {
	Instructions []projectInstruction `json:"instructions"`
	Skills       []skills.Skill       `json:"available_skills"`
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
	EasyModel   *easyModelContext  `json:"easy_model"` // Nil means easy children inherit the issuing model.
	ImageInput  bool               `json:"image_input"`
	BinaryFiles *[]nativeReadGroup `json:"binary_files"`
	ImageClick  bool               `json:"image_click"`
	Children    []tool.ChildView   `json:"children"`
	Jobs        []jobs.Metadata    `json:"live_jobs"`
	Timers      []TimerView        `json:"live_timers"`
	Project     *projectContext    `json:"project"`
}

// easyModelContext describes the configured model used for new easy children.
type easyModelContext struct {
	Model        string            `json:"model"`
	ImageInput   bool              `json:"image_input"`
	BinaryFiles  []nativeReadGroup `json:"binary_files"`
	ContextLimit int               `json:"context_limit"` // Tokens.
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
	set("easy_model", previous.EasyModel, current.EasyModel)
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
	if actor == "main" {
		next.easy = r.EasyModelChoice()
		if easy := next.easy; easy != nil {
			v.EasyModel = &easyModelContext{Model: easy.Provider + "/" + easy.Model.ID + "/" + easy.Variant, ImageInput: easy.Model.Images, BinaryFiles: nativeReadGroups(easy.Model.BinaryFileTypes()), ContextLimit: easy.Model.Budget.ContextLimit}
		}
	}
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
