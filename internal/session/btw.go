package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"ttc/internal/prompts"
	"unicode/utf8"

	contextbuild "ttc/internal/context"
	"ttc/internal/history"
	"ttc/internal/jobs"
	"ttc/internal/llm"
	"ttc/internal/render"
)

const btwInstruction = prompts.Btw

// StartBTW launches a read-only aside without interrupting the main turn. The
// frontend calls it serially with lifecycle commands, but it may overlap Run.
// Context and model selection freeze at admission. A blank session is rejected;
// session changes and exit cancel and join the task through the job supervisor.
func (r *Runtime) StartBTW(question string) (string, error) {
	if err := r.checkContext(); err != nil {
		return "", err
	}
	// Keep the frozen source and its descriptor on the same selected continuation
	// while main compaction may otherwise advance routing during aside admission.
	r.routeMu.RLock()
	defer r.routeMu.RUnlock()
	question = strings.TrimSpace(question)
	if question == "" || len(question) > 4096 || !utf8.ValidString(question) || strings.ContainsRune(question, 0) {
		return "", errors.New("/btw requires a UTF-8 question of 1–4096 bytes without NUL")
	}
	r.mu.Lock()
	persisted, active := r.persisted, r.activeCancel != nil
	prefix := append([]llm.Message(nil), r.mainPrefix...)
	selection, turn := r.prefixSelection, r.prefixTurn
	prefixSession, prefixTip := r.prefixSession, r.prefixTip
	sessionID := r.current
	if !active {
		selection = r.selection
	}
	r.mu.Unlock()
	if !persisted {
		return "", errors.New("session is empty; send a message before /btw")
	}
	saved, err := r.Store.Session(sessionID)
	if err != nil {
		return "", err
	}
	if saved.ReadOnly {
		return "", errors.New("session is read-only; load a writable session before /btw")
	}
	if saved.CompactionError != "" {
		return "", errors.New("session unusable after compaction; load or start another session before /btw")
	}
	if saved.RedoTip != 0 {
		return "", errors.New("redo is pending; use /redo before /btw, or submit a new turn to start a history branch")
	}
	if active && prefix == nil {
		return "", errors.New("main request is still preparing; retry /btw once it starts")
	}
	if !active {
		var err error
		prefix, err = r.Store.Messages(sessionID)
		if err != nil {
			return "", err
		}
		prefix = llm.ContextFor(selection, prefix)
		prefixSession, prefixTip = sessionID, saved.EntryTip
		turn = "" // Idle asides belong to the session, not a completed turn.
	}
	actor := "main/" + history.NewID("btw")
	prefix, err = r.projectInheritedSkillCatalogs(prefix, actor)
	if err != nil {
		return "", err
	}
	label := []rune(strings.Join(strings.Fields(render.Clean(question)), " "))
	if len(label) > 64 {
		label = append(label[:63], '…')
	}
	task := childTask{actor: actor, turn: turn, prompt: question, selection: selection, prefix: prefix, tools: r.Tools.Filter(readOnlyTool), aside: true, estimator: &contextbuild.Estimator{}}
	r.childStartMu.Lock()
	defer r.childStartMu.Unlock()
	count := len(r.children)
	for _, v := range r.Jobs.Live() {
		if v.Kind == "btw" {
			count++
		}
	}
	if count >= maxChildSlots {
		return "", fmt.Errorf("all %d child slots are occupied; close an idle child with job_stop or wait for a running aside to finish", maxChildSlots)
	}
	if err := r.Store.RecordActorPrefix(sessionID, turn, actor, prefixSession, prefixTip); err != nil {
		return "", err
	}
	return r.Jobs.StartTask(task.actor, jobs.TaskOptions{
		Kind: "btw", Label: string(label), Background: true,
		Inspector: btwInspector,
	}, func(ctx context.Context, stdout, stderr io.Writer) (jobs.TaskResult, error) {
		return jobs.TaskResult{}, r.runChild(ctx, task, stdout, stderr)
	})
}

// btwInspector keeps the answer as Markdown and diagnostics as bounded literal
// feedback. Acquisition, common job metadata and omission notices stay shared.
func btwInspector(v jobs.Snapshot) jobs.InspectionSpec {
	return jobs.InspectionSpec{
		Title: "/btw · " + v.Label,
		Streams: []jobs.InspectionStream{
			{Name: "stdout", LimitBytes: 65536, Markdown: true},
			{Name: "stderr", Heading: "Tool feedback", LimitBytes: 65536},
		},
	}
}
