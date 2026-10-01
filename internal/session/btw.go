package session

import (
	"context"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"scicode/internal/history"
	"scicode/internal/jobs"
	"scicode/internal/provider"
	"scicode/internal/render"
)

const btwInstruction = `Answer the user's separate /btw question with read-only analysis. The preceding
conversation is context only: another agent continues the main task. Do not
continue that task, change files, run shell commands, spawn agents, or ask
interactive questions. Only the advertised read-only tools are available.
Job handles are scoped to this child; inherited parent job IDs do not grant access.
Answer promptly and concisely, usually under 200 words. Use tools only when
needed to verify the answer. Your answer is shown in a separate Markdown popup
and does not enter the main agent's context.`

// StartBTW launches a read-only aside without interrupting the main turn. The
// frontend calls it serially with lifecycle commands, but it may overlap Run.
// Context and model selection freeze at admission. A blank session is rejected;
// session changes and exit cancel and join the task through the job supervisor.
func (r *Runtime) StartBTW(question string) (string, error) {
	question = strings.TrimSpace(question)
	if question == "" || len(question) > 4096 || !utf8.ValidString(question) || strings.ContainsRune(question, 0) {
		return "", errors.New("/btw requires a UTF-8 question of 1–4096 bytes without NUL")
	}
	r.mu.Lock()
	persisted, active := r.persisted, r.activeCancel != nil
	prefix := append([]provider.Message(nil), r.mainPrefix...)
	selection, turn := r.prefixSelection, r.prefixTurn
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
		prefix = provider.ContextFor(selection, prefix)
		turn = "" // Idle asides belong to the session, not a completed turn.
	}
	label := []rune(strings.Join(strings.Fields(render.Clean(question)), " "))
	if len(label) > 64 {
		label = append(label[:63], '…')
	}
	task := childTask{actor: "main/" + history.NewID("btw"), turn: turn, prompt: question, selection: selection, prefix: prefix, tools: r.Tools.Filter(readOnlyTool), aside: true}
	r.childStartMu.Lock()
	defer r.childStartMu.Unlock()
	count := 0
	for _, v := range r.Jobs.Live() {
		if v.Kind == "subagent" || v.Kind == "btw" {
			count++
		}
	}
	if count >= 4 {
		return "", errors.New("four child tasks are already running; wait for one or use job_stop")
	}
	return r.Jobs.StartTask(task.actor, "btw", string(label), true, false, func(ctx context.Context, stdout, stderr io.Writer) error {
		return r.runChild(ctx, task, stdout, stderr)
	})
}

func (r *Runtime) btwDetail(v jobs.Snapshot) string {
	text := "## /btw · " + render.EscapeInline(v.Label) + "\n\n"
	if v.Status != "completed" {
		text += "**" + render.EscapeInline(v.Status) + "**\n\n"
	}
	for _, stream := range []string{"stdout", "stderr"} {
		page, err := r.Jobs.Read(context.Background(), "main", v.ID, jobs.ReadOptions{Stream: stream, Cursor: "eof:-65536:bytes", Limit: 65536})
		if err != nil {
			text += "Output unavailable: " + render.Inline(err.Error()) + "\n"
			continue
		}
		output, _ := page["output"].(string)
		if page["truncated"] == true {
			text += "\nRetained " + stream + " output was truncated.\n\n"
		}
		if stream == "stdout" {
			text += render.Clean(output) + "\n"
		} else if output != "" {
			text += "\n### Tool feedback\n\n" + render.Fence(output, "text")
		}
	}
	return text
}
