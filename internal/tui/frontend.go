package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"ttc/internal/workspace"

	contextbuild "ttc/internal/context"
	"ttc/internal/graphics"
	"ttc/internal/history"
	"ttc/internal/provider"
	"ttc/internal/render"
	"ttc/internal/session"

	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
)

// Frontend wires a runtime into either a full terminal view or redirected plain I/O.
type Frontend struct {
	Screen    tcell.Screen    // Optional injected terminal screen for deterministic UI tests.
	Graphics  *graphics.Kitty // Optional protocol sink for deterministic graphics tests.
	Runtime   *session.Runtime
	Events    <-chan session.Event
	Input     io.Reader
	Output    io.Writer
	Plain     bool
	Login     provider.LoginUI
	EditInput func(context.Context, string) (string, error) // Optional editor override for embedding/tests.
	Models    []provider.ModelSpec                          // Provider catalog snapshot fetched at startup; picker actions stay local.
}
type line struct {
	actor, subagentName string // Presentation attribution; never part of the model message.
	text                string
	speaker             string // Separate left-aligned label; its body is indented by two cells.
	requestID           int64  // Stable assistant-block identity, including replayed completions.
	id                  int64
	human               bool
	system              bool
	styled              bool // Generated ANSI row (such as a streaming speaker label), independent of Markdown source.
	markdown            bool
	brief               bool // One clipped row; the complete saved result remains inspectable.
	callID              string
	questionID          string // Tagged question-status identity, retained when closed for inspection.
	jobID               string
	detail              string
	complete            bool
	awaiting            bool // Tool name announced; arguments are still being streamed.
	image               *session.ImageSnapshot
	assets              []placedImage
}
type input struct{ text string }
type operationResult struct {
	text     string
	command  string // Slash command name; empty for a model turn or editor result.
	markdown bool
	err      error
}

// Run keeps input available while a model or foreground tool runs. Enter queues FIFO.
func (f *Frontend) Run(ctx context.Context) (runErr error) {
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	var recall promptHistory
	if !f.Plain {
		prompts, err := f.Runtime.Store.PromptHistory(ctx)
		if err != nil {
			return err
		}
		for _, prompt := range prompts {
			recall.add(prompt)
		}
	}
	var screen tcell.Screen
	var tty *terminalTTY
	var g *graphics.Kitty
	graphicsWarning := ""
	var e error
	if !f.Plain {
		screen = f.Screen
		g = f.Graphics
		if screen == nil {
			screen, tty, g, e = newTerminal(ctx)
		}
		if e != nil {
			return e
		}
		if e = screen.Init(); e != nil {
			return e
		}
		defer screen.Fini()
		if tty != nil && g != nil && screen.Colors() < 1<<24 {
			g = nil
			graphicsWarning = "24-bit color is disabled; Kitty math/images require RGB colors. Remove NO_COLOR or TCELL_TRUECOLOR=disable."
		} else if tty != nil && g == nil {
			graphicsWarning = "Kitty graphics unavailable; math stays as TeX."
			if tty.graphicsError != nil {
				graphicsWarning += " " + tty.graphicsError.Error()
			}
		}
		screen.EnableMouse(tcell.MouseButtonEvents)
		screen.EnablePaste()
		screen.SetCursorStyle(tcell.CursorStyleBlinkingBar)
	}
	inputs := make(chan input, 32)
	keys := make(chan tcell.Event, 32)
	stop := make(chan struct{})
	defer close(stop)
	if screen != nil {
		go func() {
			for {
				ev := screen.PollEvent()
				if ev == nil {
					return
				}
				select {
				case keys <- ev:
				case <-stop:
					return
				}
			}
		}()
	} else {
		go func() {
			scanner := bufio.NewScanner(f.Input)
			scanner.Buffer(make([]byte, 4096), 8<<20)
			for scanner.Scan() {
				select {
				case inputs <- input{scanner.Text()}:
				case <-stop:
					return
				}
			}
			select {
			case inputs <- input{"/eof"}:
			case <-stop:
			}
		}()
	}
	done := make(chan operationResult, 1)
	busy := false
	commandBusy := false
	defer func() {
		if busy {
			f.Runtime.Interrupt()
			waitForTurn(done, f.Events)
		}
	}()
	queue := []contextbuild.Input{}
	var pendingInput *session.InputAdmission
	queueGeneration := f.Runtime.Generation()
	view := newTranscript()
	seenPrompts := map[string]int64{}
	showPrompt := func(id int64) bool {
		entry, err := f.Runtime.Store.Entry(id)
		if err != nil {
			return true // Keep the placeholder available to reveal the inspection error.
		}
		var prompt struct {
			Path      string
			RequestID int64 `json:"request_id"`
		}
		if json.Unmarshal(entry.Content, &prompt) != nil || prompt.Path == "" {
			return true
		}
		purpose := "coding"
		if prompt.RequestID > 0 {
			if err := f.Runtime.Store.DB.QueryRow("SELECT purpose FROM model_requests WHERE id=?", prompt.RequestID).Scan(&purpose); err != nil {
				return true
			}
		}
		key := entry.Actor + "\x00" + purpose
		if previous, ok := seenPrompts[key]; ok {
			// One placeholder per actor/purpose, inspecting its latest snapshot.
			if id <= previous {
				return false
			}
			for i, item := range view.lines {
				if item.id == previous {
					item.id = id
					view.replace(i, item)
					seenPrompts[key] = id
					return false
				}
			}
		}
		seenPrompts[key] = id
		return true
	}
	sidebar := newSidebar()
	fullscreen := false
	var copyView *transcript
	copyFocused := -1
	focused := 0
	dirty := true
	editorDone := make(chan operationResult, 1)
	editing := false
	editorSuspended := false
	displayView := func() *transcript {
		if fullscreen && copyView != nil {
			return copyView
		}
		return view
	}
	setFullscreen := func(enabled bool) {
		if enabled && !fullscreen {
			copyView = view.snapshot()
			copyFocused = focused
		}
		if !enabled && fullscreen && copyView != nil {
			// A frozen source anchor remains valid if its owner has not changed.
			if copyView.anchor < len(copyView.blocks) {
				block := copyView.blocks[copyView.anchor]
				owner := block.owner
				if owner < len(view.lines) && owner < len(copyView.lines) && view.lines[owner].id == copyView.lines[owner].id {
					source := copyView.anchorSource
					for n := copyView.anchor - 1; n >= 0 && copyView.blocks[n].owner == owner; n-- {
						source += len(copyView.blocks[n].text)
					}
					for n, b := range view.blocks {
						if b.owner == owner {
							view.anchor = n
							for view.anchor+1 < len(view.blocks) && view.blocks[view.anchor+1].owner == owner && source >= len(view.blocks[view.anchor].text) {
								source -= len(view.blocks[view.anchor].text)
								view.anchor++
							}
							view.anchorSource, view.resolveAnchor, view.followTail = source, true, copyView.followTail
							break
						}
					}
				}
			}
			copyView = nil
		}
		fullscreen = enabled
		dirty = true
		if screen != nil {
			if enabled {
				screen.DisableMouse()
			} else {
				screen.EnableMouse(tcell.MouseButtonEvents)
			}
		}
	}
	modal := modalState{}
	var workspaceUpdates <-chan workspace.GitInfo
	if screen != nil {
		sidebar.workspace = workspace.GitInfo{Cwd: f.Runtime.Workspace.Root}
		monitor := newWorkspaceMonitor(ctx, f.Runtime.Workspace.Root)
		defer monitor.close()
		workspaceUpdates = monitor.updates
	}
	var renderer *imageRenderer
	var renderResults <-chan renderReply
	if g != nil {
		renderer, e = newImageRenderer(ctx, g)
		if e != nil {
			return e
		}
		defer renderer.close()
		renderer.pendingClick = f.Runtime.ImageClickPending
		view.layout = renderer.layout
		renderResults = renderer.results
	}
	f.Runtime.EnableImageClicks(g != nil)
	defer f.Runtime.EnableImageClicks(false)
	var retainedQuestion *questionDialog
	viewChord := false
	draft := newComposer("")
	var pasteQuestion *questionDialog
	pasteIntoComposer := false
	deferredQuestionID := ""
	var pendingBTW []session.Event
	attachments := []contextbuild.Attachment{}
	completer := newPathCompleter(ctx)
	defer completer.close()
	completion := &completionMenu{}
	var lastQuery completionQuery
	hadQuery := false
	recallInput := func(text string) {
		draft.set(text)
		// A recalled slash command or @ path must not capture the next history key.
		if q, active := completionAt(draft, f.Runtime.Workspace.Root, f.Runtime.Generation()); active {
			completion.dismissed, completion.hasDismissed = q, true
		}
		hadQuery = false
	}
	type attachmentResult struct {
		attachment contextbuild.Attachment
		generation uint64
		err        error
	}
	attachmentDone := make(chan attachmentResult, 1)
	attaching := false
	defer func() {
		cancelRun()
		if editing {
			<-editorDone
		}
		if editorSuspended {
			runErr = errors.Join(runErr, screen.Resume())
		}
		if attaching {
			<-attachmentDone
		}
	}()
	attach := func(path string) {
		if attaching {
			addError := "An attachment is still being prepared"
			view.append(line{text: addError})
			return
		}
		attaching = true
		generation, images := f.Runtime.Generation(), f.Runtime.CurrentSelection().Model.Images
		path = f.Runtime.Workspace.Path(path)
		go func() {
			a, err := contextbuild.Snapshot(ctx, path, images)
			attachmentDone <- attachmentResult{a, generation, err}
		}()
	}
	esc := false
	started := time.Time{}
	var activity turnActivity
	eof := false
	add := func(text string, id int64) {
		view.append(line{text: text, id: id})
		if f.Plain {
			fmt.Fprintln(f.Output, render.Clean(text))
		}
	}
	names := map[string]string{}
	nameForActor := func(actor string) string {
		if actor == "" || actor == "main" {
			return ""
		}
		if name, found := names[actor]; found {
			return name
		}
		loaded, err := f.Runtime.Store.SubagentNames(ctx, f.Runtime.Current())
		if err != nil {
			add("Subagent names failed: "+err.Error(), 0)
		}
		for id, name := range loaded {
			names[id] = name
		}
		name := names[actor]
		names[actor] = name // Cache unnamed asides too; never query per chunk or frame.
		return name
	}
	attribute := func(item line, actor string) line {
		item.actor, item.subagentName = actor, nameForActor(actor)
		return item
	}
	attributeWindow := func(window *Window, actor string) {
		window.actor, window.subagentName = actor, nameForActor(actor)
	}
	plainActor := func(actor, text string) string {
		if name := nameForActor(actor); name != "" {
			return render.SubagentBadge(actor, name, 64, false) + " " + render.Clean(text)
		}
		return render.Clean(text)
	}
	addActor := func(item line, actor string) {
		view.append(attribute(item, actor))
		if f.Plain {
			fmt.Fprintln(f.Output, plainActor(actor, item.text))
		}
	}
	startEditor := func() {
		if screen == nil {
			add("Editor requires the terminal UI", 0)
			return
		}
		if err := screen.Suspend(); err != nil {
			add("Editor failed: "+err.Error(), 0)
			return
		}
		editing, editorSuspended = true, true
		editor := f.EditInput
		if editor == nil {
			editor = editDraft
		}
		text := draft.text
		go func() { text, err := editor(ctx, text); editorDone <- operationResult{text: text, err: err} }()
	}
	finishPreview := func(confirm bool) {
		if modal.preview != nil && modal.preview.pending {
			var point *[2]int
			if confirm {
				point = modal.preview.point
			}
			if err := f.Runtime.ConfirmImage(modal.preview.snapshot.ID, point); err != nil {
				add("Error: "+err.Error(), 0)
			} else {
				// Confirmation resumes the requesting agent; reveal its incoming reply.
				view.followTail = true
			}
		}
		modal.clear()
		view.invalidate()
	}
	inspect := func(id int64) {
		esc, viewChord = false, false
		historyView := modal.history
		modal.clear()
		modal.history = historyView
		modal.generation = f.Runtime.Generation()
		if !f.Plain {
			page, err := f.Runtime.Store.InspectPage(ctx, id, 0, history.InspectionPageChars)
			if err != nil {
				add("Inspection failed: "+err.Error(), 0)
				return
			}
			if page.Supported && (page.Total > page.Limit || page.LargeEnvelope) {
				modal.window = NewPagedWindow(page, func(offset int) (history.InspectionPage, error) {
					return f.Runtime.Store.InspectPage(ctx, id, offset, history.InspectionPageChars)
				})
				attributeWindow(modal.window, page.Actor)
				return
			}
		}
		if renderer != nil {
			snapshot, err := f.Runtime.ImageForEntry(id)
			if err == nil && snapshot != nil {
				if err := renderer.loadSource(*snapshot); err != nil {
					add("Image preview failed: "+err.Error(), 0)
					return
				}
				modal.loading = &previewLoad{snapshot: *snapshot, generation: f.Runtime.Generation(), entryID: id}
				modal.window = &Window{Title: "Image", Text: "Loading full image…"}
				attributeWindow(modal.window, snapshot.Actor)
				return
			}
		}
		v, e := f.Runtime.Store.Entry(id)
		if e != nil {
			add("Error: "+e.Error(), 0)
			return
		}
		text, e := f.Runtime.Store.Inspect(v)
		if e != nil {
			add("Error: "+e.Error(), 0)
			return
		}
		if f.Plain {
			fmt.Fprintln(f.Output, render.Clean(text))
		} else {
			modal.window = &Window{Title: strings.Split(f.Runtime.Store.Label(v), "\n")[0], Text: text, System: v.Kind == "status" || v.Role == "system" || v.Role == "developer", Markdown: true}
			attributeWindow(modal.window, v.Actor)
			var status struct{ Type string }
			if json.Unmarshal(v.Content, &status) == nil && status.Type == "system_prompt" {
				modal.window.Markdown = false
			}
			if json.Unmarshal(v.Content, &status) == nil && status.Type == "job_completion" {
				modal.window.Markdown, modal.window.System = true, false
			}
			if modal.window.Markdown {
				title, err := render.TerminalBriefing(modal.window.Title, 512, false)
				if err != nil {
					add("Inspection failed: "+err.Error(), 0)
					modal.clear()
					return
				}
				modal.window.Title = title
			}
			for _, item := range view.lines {
				if item.id == id && item.callID != "" && !item.complete {
					if strings.HasPrefix(item.callID, "stream:") {
						modal.window.CallID = item.callID
						modal.window.Markdown, modal.window.System = true, false
						break
					}
					modal.window.Markdown, modal.window.System = true, false
					modal.window.CallID, modal.window.JobID, modal.window.Detail, modal.window.Text = item.callID, item.jobID, item.detail, item.detail
					if item.jobID != "" && !commandBusy {
						modal.window.Text = f.Runtime.JobDetail(item.jobID, item.detail)
					}
					break
				}
			}
		}
	}
	replay := func() {
		view.reset()
		loaded, err := f.Runtime.Store.SubagentNames(ctx, f.Runtime.Current())
		if err != nil {
			add("Subagent names failed: "+err.Error(), 0)
			return
		}
		names = loaded
		saved, err := f.Runtime.CurrentSession()
		if err != nil {
			add("Session metadata failed: "+err.Error(), 0)
			return
		}
		sidebar.sessionName = saved.Name
		seenPrompts = map[string]int64{}
		entries, e := f.Runtime.Entries()
		if e != nil {
			add("Error: "+e.Error(), 0)
			return
		}
		for _, v := range entries {
			if v.InternalEvent() {
				continue
			}
			var prompt struct{ Type string }
			if json.Unmarshal(v.Content, &prompt) == nil && prompt.Type == "system_prompt" && !showPrompt(v.ID) {
				continue
			}
			item := attribute(line{text: f.Runtime.Store.Label(v), id: v.ID, system: v.Kind == "status" || v.Role == "system"}, v.Actor)
			if v.Kind == "message" && v.Role == "developer" {
				item.system = true
			}
			var status struct{ Type string }
			if v.Kind == "tool_result" || json.Unmarshal(v.Content, &status) == nil && status.Type == "job_completion" {
				item.markdown, item.brief, item.system = true, true, false
			}
			if v.Kind == "message" && v.Role == "user" && v.Actor == "main" && v.Visible {
				var message provider.Message
				if err := json.Unmarshal(v.Content, &message); err == nil {
					item.human = !message.Runtime
				}
			}
			if (v.Kind == "message" || v.Kind == "summary") && v.Role == "assistant" {
				var message provider.Message
				if err := json.Unmarshal(v.Content, &message); err == nil {
					title := "assistant"
					item.text, item.speaker, item.markdown, item.complete, item.requestID = message.Content, title, true, true, message.RequestID
				}
			}
			if v.Kind == "tool_result" && renderer != nil {
				snapshot, err := f.Runtime.ImageForEntry(v.ID)
				if err == nil {
					item.image = snapshot
				}
			}
			view.append(item)
		}
		if renderer != nil {
			cards, err := f.Runtime.PendingImages()
			if err != nil {
				add("Pending image display failed: "+err.Error(), 0)
			}
			for _, card := range cards {
				present := false
				for i, item := range view.lines {
					if item.image != nil && item.image.ID == card.Snapshot.ID {
						present = true
						break
					}
					if item.id == card.EntryID {
						item = attribute(line{text: "**image_show** · " + render.Inline(card.Snapshot.Path), id: card.EntryID, callID: card.Snapshot.ID, markdown: true, brief: true, image: &card.Snapshot}, card.Snapshot.Actor)
						view.replace(i, item)
						present = true
						break
					}
				}
				if !present {
					view.append(attribute(line{text: "**image_show** · " + render.Inline(card.Snapshot.Path), id: card.EntryID, callID: card.Snapshot.ID, markdown: true, brief: true, image: &card.Snapshot}, card.Snapshot.Actor))
				}
			}
		}
		focused = len(view.lines) - 1
		view.followTail = true
	}
	start := func(input *session.InputAdmission) {
		busy = true
		started = time.Now()
		activity = turnActivity{}
		pendingInput = input
		go func() {
			if input == nil {
				done <- operationResult{err: f.Runtime.Run(nil)}
			} else {
				done <- operationResult{err: f.Runtime.RunInput(input)}
			}
		}()
	}
	openModelMenu := func() {
		esc, viewChord = false, false
		if len(f.Models) == 0 {
			add("Error: model catalog unavailable", 0)
			return
		}
		if f.Plain {
			var b strings.Builder
			for _, model := range f.Models {
				fmt.Fprintf(&b, "%s · %s · variants=%v · default=%s\n", model.ID, model.Name, model.Variants, model.DefaultVariant)
			}
			add(b.String()+"Select with /model ID [VARIANT]", 0)
			return
		}
		selected := f.Runtime.ModelChoice()
		modal.clear()
		modal.menu = newModelMenu(f.Models, selected)
		modal.question = nil
		modal.window = &modal.menu.Window
	}
	questionRow := -1 // Only the singleton's live status row needs reconciliation.
	syncQuestionState := func(form *session.QuestionForm) {
		activity.syncQuestion(form, time.Now())
		if questionRow >= len(view.lines) || questionRow >= 0 && view.lines[questionRow].questionID == "" {
			questionRow = -1
		}
		if questionRow >= 0 {
			item := view.lines[questionRow]
			label := "Question closed · " + item.questionID
			live := form != nil && form.ID == item.questionID
			if live {
				label = questionStatus(*form)
			}
			if item.text != label {
				item.text = label
				view.replace(questionRow, item)
				if f.Plain {
					fmt.Fprintln(f.Output, label)
				}
			}
			if !live {
				questionRow = -1
			}
		}
		if form != nil && questionRow < 0 {
			view.append(attribute(line{text: questionStatus(*form), id: form.EntryID, system: true, questionID: form.ID}, "main"))
			questionRow = len(view.lines) - 1
		}
		if retainedQuestion != nil && (form == nil || form.ID != retainedQuestion.form.ID) {
			retainedQuestion = nil
		}
		if modal.question != nil && (form == nil || form.ID != modal.question.form.ID || form.Dismissed) {
			modal.clear()
		}
		if form == nil || form.ID != deferredQuestionID || form.Dismissed {
			deferredQuestionID = ""
		}
	}
	// Automatic presentation never changes dismissal. Only an explicit user
	// reopen may restore a hidden form; delayed initial events cannot do so.
	showQuestion := func(form *session.QuestionForm) {
		esc, viewChord = false, false
		deferredQuestionID = ""
		if f.Plain {
			data, _ := json.MarshalIndent(form.Questions, "", "  ")
			add("Pending question · "+form.ID+"\n"+string(data), form.EntryID)
			return
		}
		if retainedQuestion == nil || retainedQuestion.form.ID != form.ID {
			retainedQuestion = newQuestionDialog(*form)
		}
		retainedQuestion.form = *form
		modal.clear()
		modal.generation = f.Runtime.Generation()
		modal.question = retainedQuestion
		modal.window = &retainedQuestion.Window
	}
	openQuestions := func(id string) {
		esc, viewChord = false, false
		form := f.Runtime.PendingQuestion()
		if form == nil {
			add("No pending question", 0)
			return
		}
		if id != "" && form.ID != id {
			add("Error: pending question not found · "+id, 0)
			return
		}
		if err := f.Runtime.ReopenQuestion(form.ID); err != nil {
			add("Reopen failed: "+err.Error(), 0)
			return
		}
		form = f.Runtime.PendingQuestion()
		syncQuestionState(form)
		if form != nil {
			showQuestion(form)
		}
	}
	openSessionMenu := func() {
		esc, viewChord = false, false
		if busy {
			add("Session picker requires an idle turn", 0)
			return
		}
		list, err := f.Runtime.Store.Sessions(f.Runtime.Workspace.Root)
		if err != nil {
			add("Sessions failed: "+err.Error(), 0)
			return
		}
		modal.clear()
		modal.sessions = newSessionMenu(list, f.Runtime.Current(), time.Now())
		modal.window = &modal.sessions.Window
	}
	openHistoryMenu := func() {
		esc, viewChord = false, false
		if busy {
			add("History picker requires an idle turn", 0)
			return
		}
		tree, err := f.Runtime.History()
		if err != nil {
			add("History failed: "+err.Error(), 0)
			return
		}
		modal.clear()
		modal.history = newHistoryMenu(tree)
		modal.window = &modal.history.Window
	}
	openBackgroundMenu := func() {
		if commandBusy {
			return
		}
		list := f.Runtime.Jobs.Foreground("main")
		if len(list) == 0 {
			return
		}
		modal.clear()
		modal.background = newBackgroundMenu(list)
		modal.window = &modal.background.Window
	}
	selectModel := func(id, variant string) {
		selection, err := provider.Resolve(f.Runtime.CurrentSelection().Provider, f.Models, id, variant)
		if err != nil {
			add("Error: "+err.Error(), 0)
			return
		}
		if err := f.Runtime.RequestModel(selection); err != nil {
			add("Model selection failed: "+err.Error(), 0)
			return
		}
		add("Model selected for next tool boundary · "+id+" · "+selection.Variant, 0)
	}
	add("TTC · Linux terminal agent · /help", 0)
	replay()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if generation := f.Runtime.Generation(); generation != queueGeneration {
			queue = nil
			pendingInput = nil
			queueGeneration = generation
			activity = turnActivity{}
			retainedQuestion = nil
			deferredQuestionID = ""
			questionRow = -1
			if modal.question != nil {
				modal.clear()
			}
		}
		syncQuestionState(f.Runtime.PendingQuestion())
		if !f.Plain && !fullscreen && !editing && !draft.pasting && modal.empty() && len(pendingBTW) > 0 {
			event := pendingBTW[0]
			pendingBTW = pendingBTW[1:]
			if event.Generation == f.Runtime.Generation() {
				modal.generation = event.Generation
				modal.window = &Window{Title: "/btw · read-only answer", Text: event.Text, Markdown: true}
			}
		}
		if deferredQuestionID != "" && !fullscreen && !editing && !draft.pasting && modal.empty() {
			id := deferredQuestionID
			deferredQuestionID = ""
			if form := f.Runtime.PendingQuestion(); form != nil && form.ID == id && !form.Dismissed {
				showQuestion(form)
			}
		}
		if !busy {
			if event, err := f.Runtime.ApplyModel(""); err != nil {
				return err
			} else if event.Kind != "" {
				add(event.Text, event.EntryID)
				view.lines[len(view.lines)-1].system = true
			}
			if f.Runtime.HasNotifications() {
				start(nil)
			} else if len(queue) > 0 {
				input := f.Runtime.PrepareInput(queue[0])
				queue[0] = contextbuild.Input{}
				queue = queue[1:]
				start(input)
			} else if eof {
				return nil
			}
		}
		draft.completion = nil
		if screen != nil && modal.empty() && !fullscreen && !editing {
			q, active := completionAt(draft, f.Runtime.Workspace.Root, f.Runtime.Generation())
			if active && !(completion.hasDismissed && q == completion.dismissed) {
				if !hadQuery || q != lastQuery {
					lastQuery, hadQuery = q, true
					completion.selected = 0
					completion.result = completionResult{query: q}
					if q.marker == '/' {
						completion.result = commandCompletions(q)
					} else {
						completer.query(q)
					}
				}
				draft.completion = completion
			} else {
				hadQuery = false
			}
		}
		if screen != nil && !editing && (!fullscreen || dirty) {
			if modal.menu != nil {
				w, h := screen.Size()
				width, height := windowContentSize(w, h, modal.window)
				modal.menu.reveal(width, height)
			}
			if modal.commands != nil {
				w, h := screen.Size()
				width, height := windowContentSize(w, h, modal.window)
				modal.commands.reveal(width, height)
			}
			if modal.prompts != nil {
				w, h := screen.Size()
				width, height := windowContentSize(w, h, modal.window)
				modal.prompts.reveal(width, height)
			}
			if modal.sessions != nil {
				w, h := screen.Size()
				width, height := windowContentSize(w, h, modal.window)
				modal.sessions.reveal(width, height)
			}
			if modal.history != nil && modal.window == &modal.history.Window {
				w, h := screen.Size()
				width, height := windowContentSize(w, h, modal.window)
				modal.history.reveal(width, height)
			}
			if modal.background != nil {
				w, h := screen.Size()
				width, height := windowContentSize(w, h, modal.window)
				modal.background.reveal(width, height)
			}
			if modal.question != nil {
				w, h := screen.Size()
				width, height := windowContentSize(w, h, modal.window)
				modal.question.reveal(width, height)
			}
			width, height := screen.Size()
			sidebar.bounds(width, height, fullscreen)
			if !commandBusy && sidebar.width > 0 {
				sidebar.update(f.Runtime.UsageSnapshot(), f.Runtime.Jobs.Live(), f.Runtime.LiveTimers())
			}
			if renderer != nil {
				var size tcell.WindowSize
				if tty != nil {
					if measured, err := tty.WindowSize(); err == nil {
						size = measured
					}
				}
				changed, warning := renderer.updateCellDimensions(size)
				if changed {
					view.invalidate()
					if modal.preview != nil {
						modal.preview.cellWidth, modal.preview.cellHeight = renderer.cellWidth, renderer.cellHeight
					}
				}
				if warning != "" {
					view.append(line{text: warning, system: true})
				}
				g.Begin()
			}
			if modal.window != nil && modal.window.JobID != "" && !commandBusy {
				modal.window.Text = f.Runtime.JobDetail(modal.window.JobID, modal.window.Detail)
			}
			drawFocus := focused
			if fullscreen {
				drawFocus = copyFocused
			}
			previewLimit := max(0, height-4)
			if fullscreen {
				previewLimit = 0
			}
			steerCount, steers := f.Runtime.SteeringPreview(previewLimit)
			indicator := ""
			if busy {
				indicator = fmt.Sprintf("%s · %d queued · Esc Esc interrupt · Ctrl+C exit", activity.indicator(time.Now(), started), len(queue)+steerCount)
			}
			if err := draw(screen, displayView(), sidebar, fullscreen, modal.preview, renderer, drawFocus, draft, len(attachments), queue, steers, indicator, modal.window, f.Runtime.CurrentSelection()); err != nil {
				return err
			}
			if renderer != nil {
				if err := g.End(); err != nil {
					return err
				}
			}
		}
		dirty = false
		submit := ""
		haveInput := false
		steering := false
		select {
		case <-ctx.Done():

			f.Runtime.Interrupt()
			if busy {
				waitForTurn(done, f.Events)
				busy = false
			}
			return ctx.Err()
		case result := <-renderResults:
			if result.source {
				if modal.loading != nil && modal.loading.snapshot.ID == result.key && modal.loading.generation == f.Runtime.Generation() {
					if result.err != nil {
						modal.window.Text = "Image preview failed: " + result.err.Error()
					} else {
						modal.preview = newImagePreview(modal.loading.snapshot, result.pixels, f.Runtime.ImageClickPending(result.key), renderer.cellWidth, renderer.cellHeight)
						attributeWindow(&modal.preview.Window, modal.loading.snapshot.Actor)
						detailEntry, err := f.Runtime.Store.Entry(modal.loading.entryID)
						if err == nil {
							modal.preview.detailText, err = f.Runtime.Store.Inspect(detailEntry)
						}
						if err != nil {
							modal.preview.detailText = "Inspection failed: " + err.Error()
						}
						modal.window = &modal.preview.Window
					}
					modal.loading = nil
				}
			} else {
				if renderer.accept(result) {
					view.invalidate()
					failure := result.err
					if result.backend == "" {
						failure = renderer.ready[result.key].err
					}
					if failure != nil {
						view.append(line{text: "Warning: math/image rendering failed: " + render.Clean(failure.Error()), system: true})
					}
				}
			}
		case result := <-completer.results:
			q, active := completionAt(draft, f.Runtime.Workspace.Root, f.Runtime.Generation())
			if active && result.query == q && modal.empty() && !fullscreen {
				completion.result, completion.selected = result, 0
				if result.err != nil {
					completion.result.items = nil
				}
			}
		case result := <-attachmentDone:
			attaching = false
			if result.generation == f.Runtime.Generation() {
				if result.err != nil {
					add("Attachment failed: "+result.err.Error(), 0)
				} else {
					attachments = append(attachments, result.attachment)
					add("Attached · "+result.attachment.Path, 0)
				}
			}
		case result := <-editorDone:
			editing = false
			if err := screen.Resume(); err != nil {
				return fmt.Errorf("resume terminal after editor: %w", err)
			}
			editorSuspended = false
			screen.EnablePaste()
			screen.SetCursorStyle(tcell.CursorStyleBlinkingBar)
			if fullscreen {
				screen.DisableMouse()
			} else {
				screen.EnableMouse(tcell.MouseButtonEvents)
			}
			screen.Sync()
			dirty = true
			if result.err != nil {
				add("Editor failed: "+result.err.Error(), 0)
			} else {
				draft.set(result.text)
			}
		case result := <-done:
			activity = turnActivity{}
			if modal.generation != 0 && modal.generation != f.Runtime.Generation() {
				modal.clear()
			}
			busy = false
			commandBusy = false
			if result.err != nil {
				add("Error: "+result.err.Error(), 0)
			} else if result.command != "" {
				replay()
				if f.Plain {
					add(result.text, 0)
				} else {
					switch result.command {
					case "/jobs", "/timers", "/compact":
						if modal.empty() {
							modal.window = &Window{Title: "Command result", Text: result.text, Markdown: result.markdown}
						}
					case "/load", "/branch":
						// The loaded conversation is the success feedback.
					default:
						add(result.text, 0)
					}
				}
			}
			focused = len(view.lines) - 1
		case event := <-f.Events:
			// Aside results belong to the live generation, which survives
			// compaction. Explicit session changes still discard old popups.
			if event.Generation != f.Runtime.Generation() || event.Kind != "btw_result" && event.SessionID != "" && event.SessionID != f.Runtime.Current() {
				continue
			}
			activity.observe(event, time.Now())
			switch event.Kind {
			case "continuation":
				// Rebuild the committed history before rendering subsequent coding
				// events. Compaction keeps drafts, queued input and live dialogs.
				following, firstLine := view.followTail, view.firstLine
				replay()
				if !following {
					view.followTail, view.firstLine = false, firstLine
				}
				if f.Plain {
					fmt.Fprintln(f.Output, render.Clean(event.Text))
				}
			case "btw_result":
				if f.Plain {
					fmt.Fprintln(f.Output, render.Clean(event.Text))
				} else {
					// Older answers remain inspectable in history if the user keeps
					// a dialog open while launching many independent asides.
					if len(pendingBTW) == 16 {
						pendingBTW = pendingBTW[1:]
					}
					pendingBTW = append(pendingBTW, event)
				}
			case "usage":
				continue
			case "session_name":
				saved, err := f.Runtime.CurrentSession()
				if err != nil {
					add("Session metadata failed: "+err.Error(), 0)
				} else {
					sidebar.sessionName = saved.Name
				}
				continue
			case "tool_pending":
				addActor(line{text: event.Text, id: event.EntryID, callID: event.CallID, awaiting: true, system: true}, event.Actor)
			case "tool_stream_end":
				for key, index := range view.calls {
					if strings.HasPrefix(key, event.PendingKey) && view.lines[index].awaiting {
						item := view.lines[index]
						item.awaiting = false
						item.text = strings.Replace(item.text, "awaiting ", "Tool "+event.Text+" · ", 1)
						view.replace(index, item)
					}
				}
			case "image":
				if renderer != nil && event.Image != nil {
					if index, ok := view.calls[event.CallID]; ok {
						item := view.lines[index]
						item.image = event.Image
						view.replace(index, item)
					} else {
						view.append(attribute(line{text: "**image_show** · " + render.Inline(event.Image.Path), callID: event.CallID, markdown: true, brief: true, image: event.Image}, event.Actor))
					}
				}
			case "tool_update", "tool":
				item := attribute(line{text: event.Text, id: event.EntryID, callID: event.CallID, jobID: event.JobID, detail: event.Detail, markdown: true, brief: true, complete: event.Kind == "tool"}, event.Actor)
				if !view.publish(item, event.PendingKey, false) {
					break
				}
				if f.Plain {
					brief, err := render.TerminalBriefing(event.Text, 500, false)
					if err != nil {
						return fmt.Errorf("render tool briefing: %w", err)
					}
					fmt.Fprintln(f.Output, plainActor(event.Actor, brief))
				}
				if modal.window != nil && modal.window.CallID != "" && (modal.window.CallID == event.CallID || modal.window.CallID == event.PendingKey) {
					modal.window.CallID = event.CallID
					if event.Kind == "tool" {
						inspect(event.EntryID)
					} else {
						modal.window.JobID, modal.window.Detail, modal.window.Text = event.JobID, event.Detail, event.Detail
						if modal.window.JobID != "" && !commandBusy {
							modal.window.Text = f.Runtime.JobDetail(modal.window.JobID, modal.window.Detail)
						}
					}
				}
			case "job":
				view.append(attribute(line{text: event.Text, id: event.EntryID, markdown: true, brief: true}, event.Actor))
				if f.Plain {
					brief, err := render.TerminalBriefing(event.Text, 500, false)
					if err != nil {
						return fmt.Errorf("render job briefing: %w", err)
					}
					fmt.Fprintln(f.Output, plainActor(event.Actor, brief))
				}
			case "delta":
				_, existing := view.requests[event.RequestID]
				if view.publish(attribute(line{text: event.Text, speaker: "assistant", requestID: event.RequestID}, event.Actor), "", true) && f.Plain {
					if !existing && nameForActor(event.Actor) != "" {
						fmt.Fprintln(f.Output, plainActor(event.Actor, "assistant"))
					}
					fmt.Fprint(f.Output, event.Text)
				}
			case "assistant":
				speaker := "assistant"
				index, streamed := view.requests[event.RequestID]
				streamed = streamed && !view.lines[index].complete
				if view.publish(attribute(line{text: event.Text, speaker: speaker, requestID: event.RequestID, id: event.EntryID, complete: true, markdown: true}, event.Actor), "", false) && f.Plain {
					if streamed {
						fmt.Fprintln(f.Output)
					} else {
						fmt.Fprintln(f.Output, plainActor(event.Actor, speaker+" · "+event.Text))
					}
				}
				if graphicsWarning != "" {
					hasMath := false
					render.Math(event.Text, func(tex string, block bool) string { hasMath = true; return tex })
					if hasMath {
						view.append(line{text: "Warning: " + graphicsWarning, system: true})
						graphicsWarning = ""
					}
				}
			case "question":
				if event.Question == nil {
					return fmt.Errorf("question event is missing its form")
				}
				form := f.Runtime.PendingQuestion()
				syncQuestionState(form)
				if form == nil || form.ID != event.Question.ID {
					break // Ignore stale publication; tool intents remain inspectable.
				}
				if f.Plain {
					fmt.Fprintln(f.Output, render.Clean(event.Text))
				}
				if !f.Plain && !form.Dismissed && modal.question == nil {
					if !modal.empty() || draft.pasting || fullscreen || editing {
						deferredQuestionID = form.ID
					} else {
						showQuestion(form)
					}
				}
			case "question_closed":
				// Runtime state may already contain the next round. A delayed
				// closure must not clear that round's activity or dialog.
				syncQuestionState(f.Runtime.PendingQuestion())
			case "system_prompt":
				if showPrompt(event.EntryID) {
					addActor(line{text: event.Text, id: event.EntryID, system: true}, event.Actor)
				}
			case "runtime_context", "status", "wake":
				addActor(line{text: event.Text, id: event.EntryID, system: true}, event.Actor)
			default:
				addActor(line{text: event.Text, id: event.EntryID, human: event.Human}, event.Actor)
			}
			focused = len(view.lines) - 1
		case v := <-inputs:
			submit = v.text
			haveInput = true
		case ev := <-keys:
			if editing {
				continue
			}
			dirty = true
			switch ev := ev.(type) {
			case *tcell.EventPaste:
				if modal.prompts != nil {
					modal.prompts.query.pasting = ev.Start()
					continue
				}
				draft.pasting = ev.Start()
				if ev.Start() {
					esc, viewChord = false, false
					pasteQuestion = modal.question
					pasteIntoComposer = pasteQuestion == nil && modal.menu == nil && modal.commands == nil && modal.preview == nil
				}
				if pasteQuestion != nil {
					esc = false
					pasteQuestion.pasting = ev.Start()
					if ev.Start() && pasteQuestion.tab < len(pasteQuestion.answers) {
						pasteQuestion.manualScroll = false
						a := &pasteQuestion.answers[pasteQuestion.tab]
						a.choice = len(pasteQuestion.form.Questions[pasteQuestion.tab].Options)
						a.useCustom, a.editing = true, true
						pasteQuestion.update()
					}
				}
				if !ev.Start() {
					pasteQuestion, pasteIntoComposer = nil, false
				}
			case *tcell.EventResize:
				screen.Sync()
			case *tcell.EventMouse:
				if fullscreen {
					dirty = false
					continue // Ignore reports already queued before terminal selection mode.
				}
				x, y := ev.Position()
				if modal.preview != nil {
					close, confirm := modal.preview.mouse(ev)
					if close {
						finishPreview(confirm)
					}
					continue
				}
				if modal.window != nil {
					if modal.prompts != nil {
						w, h := screen.Size()
						modal.prompts.mouse(ev, w, h)
						continue
					}
					if modal.background != nil {
						w, h := screen.Size()
						modal.background.mouse(ev, w, h)
						continue
					}
					if modal.history != nil && modal.window == &modal.history.Window {
						w, h := screen.Size()
						modal.history.mouse(ev, w, h)
						continue
					}
					if modal.sessions != nil {
						w, h := screen.Size()
						modal.sessions.mouse(ev, w, h)
					}
					if ev.Buttons()&tcell.WheelUp != 0 {
						modal.window.Scroll = max(0, modal.window.Scroll-3)
						if modal.question != nil {
							modal.question.manualScroll = true
						}
					}
					if ev.Buttons()&tcell.WheelDown != 0 {
						modal.window.Scroll += 3
						if modal.question != nil {
							modal.question.manualScroll = true
						}
					}
					continue
				}
				if consumed, action := sidebar.mouse(ev); consumed {
					if action.workspace {
						modal.clear()
						modal.window = &Window{Title: "Session & workspace", Text: sidebar.detail()}
					}
					if action.jobID != "" && !commandBusy {
						modal.clear()
						modal.window = &Window{Title: "Running job", Text: f.Runtime.JobDetail(action.jobID, ""), Markdown: true, JobID: action.jobID}
						if job, err := f.Runtime.Jobs.View("main", action.jobID); err == nil {
							attributeWindow(modal.window, job.Owner)
						} else {
							modal.window.Text = "Job inspection failed: " + err.Error()
						}
					}
					continue
				}
				if ev.Buttons()&tcell.WheelUp != 0 {
					view.scroll(-3, len(view.visible))
				}
				if ev.Buttons()&tcell.WheelDown != 0 {
					view.scroll(3, len(view.visible))
				}
				width, _ := screen.Size()
				width = sidebar.bounds(width, sidebar.height, fullscreen)
				if ev.Buttons()&tcell.Button1 != 0 && x >= 0 && x < width-1 {
					if id := view.entryAt(y); id > 0 {
						inspect(id)
					}
				}
			case *tcell.EventKey:
				// Legacy tty Alt+Enter is ESC CR; tcell decodes it as Alt+Ctrl+M.
				if ev.Key() == tcell.KeyRune && (ev.Rune() == 'm' || ev.Rune() == 'j') && ev.Modifiers()&(tcell.ModAlt|tcell.ModCtrl) == tcell.ModAlt|tcell.ModCtrl {
					ev = tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModAlt)
				}
				if ev.Key() == tcell.KeyCtrlC {
					submit, haveInput = "/quit", true
					break
				}
				if modal.prompts != nil {
					w, h := screen.Size()
					_, height := windowContentSize(w, h, modal.window)
					text, closed := modal.prompts.key(ev, height)
					if closed {
						modal.clear()
						if text != "" {
							recallInput(text)
							recall.index, recall.draft = len(recall.entries), ""
						}
					}
					continue
				}
				if draft.pasting && (pasteQuestion == nil || modal.question != pasteQuestion) {
					if pasteIntoComposer {
						modal.clear()
						draft.pasteKey(ev)
					}
					continue
				}
				if modal.preview != nil {
					close, confirm := modal.preview.key(ev)
					if close {
						finishPreview(confirm)
					}
					continue
				}
				if modal.question != nil {
					esc = false
					w, h := screen.Size()
					_, height := windowContentSize(w, h, modal.window)
					answers, dismissed := modal.question.key(ev, height)
					if dismissed {
						if err := f.Runtime.DismissQuestion(modal.question.form.ID); err != nil {
							modal.question.errorText = err.Error()
							modal.question.update()
						} else {
							modal.clear()
							syncQuestionState(f.Runtime.PendingQuestion())
							esc = false
						}
					}
					if answers != nil {
						if err := f.Runtime.AnswerQuestion(modal.question.form.ID, answers); err != nil {
							modal.question.errorText = err.Error()
							modal.question.update()
						} else {
							retainedQuestion = nil
							modal.clear()
						}
					}
					continue
				}
				if viewChord {
					viewChord = false
					if ev.Key() == tcell.KeyRune && (ev.Rune() == 'e' || ev.Rune() == 'E') {
						startEditor()
						continue
					}
					if ev.Key() == tcell.KeyRune && (ev.Rune() == 'f' || ev.Rune() == 'F') {
						setFullscreen(!fullscreen)
						sidebar.overlay = false
						continue
					}
					if ev.Key() == tcell.KeyRune && (ev.Rune() == 's' || ev.Rune() == 'S') {
						sidebar.overlay = !sidebar.overlay
						continue
					}
					if ev.Key() == tcell.KeyRune && (ev.Rune() == 'm' || ev.Rune() == 'M') {
						openModelMenu()
						continue
					}
					if ev.Key() == tcell.KeyRune && (ev.Rune() == 'l' || ev.Rune() == 'L') {
						openSessionMenu()
						continue
					}
					if ev.Key() == tcell.KeyRune && (ev.Rune() == 'g' || ev.Rune() == 'G') {
						openHistoryMenu()
						continue
					}
					if ev.Key() == tcell.KeyRune {
						switch ev.Rune() {
						case '?':
							openQuestions("")
							continue
						case 'j', 'J':
							submit = "/jobs"
						case 't', 'T':
							submit = "/timers"
						case 'n', 'N':
							submit = "/new"
						case 'q', 'Q':
							submit = "/quit"
						}
						if submit != "" {
							modal.clear()
							setFullscreen(false)
							haveInput = true
							break
						}
					}
				}
				if ev.Key() == tcell.KeyCtrlX {
					viewChord = true
					continue
				}
				if ev.Key() == tcell.KeyCtrlP {
					modal.clear()
					modal.generation = f.Runtime.Generation()
					modal.commands = newCommandMenu()
					modal.window = &modal.commands.Window
					continue
				}
				if ev.Key() == tcell.KeyCtrlR {
					modal.clear()
					setFullscreen(false)
					modal.generation = f.Runtime.Generation()
					modal.prompts = newPromptSearch(recall.entries)
					modal.window = &modal.prompts.Window
					continue
				}
				if modal.commands != nil {
					w, h := screen.Size()
					_, height := windowContentSize(w, h, modal.window)
					command, closed := modal.commands.key(ev, height)
					if closed {
						modal.clear()
						if command != "" {
							draft.set(command)
							setFullscreen(false)
						}
					}
					continue
				}
				if modal.menu != nil {
					esc = false
					w, h := screen.Size()
					_, height := windowContentSize(w, h, modal.window)
					id, variant, closed := modal.menu.key(ev, height)
					if closed {
						modal.clear()
						if id != "" {
							selectModel(id, variant)
						}
					}
					continue
				}
				if modal.background != nil {
					id, closed := modal.background.key(ev)
					if closed {
						modal.clear()
						if id != "" {
							if _, err := f.Runtime.Jobs.Promote("main", id); err != nil {
								add("Background failed: "+err.Error(), 0)
							}
						}
					}
					continue
				}
				if modal.sessions != nil {
					esc = false
					w, h := screen.Size()
					_, height := windowContentSize(w, h, modal.window)
					id, closed := modal.sessions.key(ev, height)
					if closed {
						modal.clear()
						if id != "" {
							submit = "/load " + id
							haveInput = true
							setFullscreen(false)
						}
					}
					if !haveInput {
						continue
					}
					break // Selected reload owns Enter; preserve the composer draft.
				}
				if modal.history != nil && modal.window == &modal.history.Window {
					esc = false
					w, h := screen.Size()
					_, height := windowContentSize(w, h, modal.window)
					action, id := modal.history.key(ev, height)
					switch action {
					case "close":
						modal.clear()
					case "inspect":
						inspect(id)
					case "restore":
						modal.clear()
						submit, haveInput = fmt.Sprintf("/branch %d", id), true
						setFullscreen(false)
					}
					if !haveInput {
						continue
					}
					break // Preserve the composer while selecting a branch.
				}
				if modal.window != nil {
					if ev.Key() == tcell.KeyEscape {
						if modal.history != nil {
							modal.window = &modal.history.Window
						} else {
							modal.clear()
						}
						esc = false
						continue
					}
					w, h := screen.Size()
					_, height := windowContentSize(w, h, modal.window)
					if modal.window.Key(ev, height) {
						continue
					}
				}
				if sidebar.overlay && modal.window == nil && sidebar.key(ev) {
					continue
				}
				if draft.completion != nil && len(completion.result.items) > 0 {
					switch ev.Key() {
					case tcell.KeyUp:
						completion.selected = max(0, completion.selected-1)
						continue
					case tcell.KeyDown:
						completion.selected = min(len(completion.result.items)-1, completion.selected+1)
						continue
					case tcell.KeyEscape:
						completion.dismissed, completion.hasDismissed = completion.result.query, true
						hadQuery = false
						continue
					case tcell.KeyTab, tcell.KeyEnter:
						q := completion.result.query
						exactCommand := q.marker == '/' && "/"+q.prefix == completion.result.items[completion.selected].value
						if ev.Key() != tcell.KeyEnter || !exactCommand {
							if path, ok := completion.accept(&draft); ok && path != "" {
								attach(path)
							}
							continue
						}
					}
				}
				if ev.Key() == tcell.KeyEscape {
					if modal.window != nil {
						modal.clear()
					} else if sidebar.overlay {
						sidebar.overlay = false
					} else if fullscreen {
						setFullscreen(false)
					} else if !view.followTail {
						view.followTail = true
					} else if esc {
						f.Runtime.Interrupt()
						esc = false
					} else {
						esc = true
					}
					continue
				}
				if haveInput {
					break
				}
				esc = false
				if ev.Key() == tcell.KeyCtrlB && modal.empty() && !commandBusy {
					foreground := f.Runtime.Jobs.Foreground("main")
					if len(foreground) > 0 {
						for _, job := range foreground {
							if _, err := f.Runtime.Jobs.Promote("main", job.ID); err != nil {
								add("Background failed: "+err.Error(), 0)
							}
						}
						continue
					}
				}
				if draft.key(ev) {
					modal.clear()
					continue
				}
				switch ev.Key() {
				case tcell.KeyCtrlD:
					displayView().pageDown(len(displayView().visible))
				case tcell.KeyCtrlU:
					displayView().scroll(-max(1, len(displayView().visible)/2), len(displayView().visible))
				case tcell.KeyUp:
					if ev.Modifiers()&tcell.ModAlt != 0 {
						if fullscreen {
							copyFocused = max(0, copyFocused-1)
						} else {
							focused = max(0, focused-1)
						}
					} else {
						recallInput(recall.move(draft.text, -1))
					}
				case tcell.KeyDown:
					if ev.Modifiers()&tcell.ModAlt != 0 {
						if fullscreen {
							copyFocused = min(len(copyView.lines)-1, copyFocused+1)
						} else {
							focused = min(len(view.lines)-1, focused+1)
						}
					} else {
						recallInput(recall.move(draft.text, 1))
					}
				case tcell.KeyTab:
					target := focused
					if fullscreen {
						target = copyFocused
					}
					if target >= 0 && target < len(displayView().lines) && displayView().lines[target].id > 0 {
						inspect(displayView().lines[target].id)
					}
				case tcell.KeyEnter:
					if attaching {
						add("Preparing attachment; press Enter when ready", 0)
						continue
					}
					target := focused
					if fullscreen {
						target = copyFocused
					}
					if draft.text == "" && target >= 0 && target < len(displayView().lines) && displayView().lines[target].id > 0 {
						inspect(displayView().lines[target].id)
					} else {
						submit = draft.text
						steering = busy && !commandBusy && ev.Modifiers()&tcell.ModAlt != 0
						setFullscreen(false)
						draft.set("")
						haveInput = true
						view.followTail = true
					}
				}
			}
		case v := <-workspaceUpdates:
			sidebar.workspace = v
		case <-ticker.C:
		}
		if !haveInput {
			continue
		}
		text := strings.TrimSpace(submit)
		if text == "" {
			continue
		}
		if text != "/eof" {
			recall.add(submit)
		}
		if text == "/quit" {
			f.Runtime.Interrupt()
			if busy {
				waitForTurn(done, f.Events)
				busy = false
			}
			return nil
		}
		if text == "/eof" {
			eof = true
			continue
		}
		if text == "/cancel-queue" || text == "/cancel-steer" {
			var restored contextbuild.Input
			var err error
			if text == "/cancel-queue" {
				if len(queue) == 0 {
					if pendingInput == nil {
						err = errors.New("no queued prompt to cancel")
					} else {
						restored, err = pendingInput.Cancel()
					}
				} else {
					last := len(queue) - 1
					restored = queue[last]
					queue[last] = contextbuild.Input{}
					queue = queue[:last]
				}
			} else {
				restored, err = f.Runtime.CancelSteer()
			}
			if err != nil {
				add("Error: "+err.Error(), 0)
			} else {
				recallInput(restored.Text)
				attachments = restored.Attachments
				add("Cancelled pending input · restored to composer", 0)
			}
			continue
		}
		if text == "/editor" {
			startEditor()
			continue
		}
		if text == "/help" {
			result, err := f.Runtime.Command(text)
			if err != nil {
				add("Error: "+err.Error(), 0)
			} else if f.Plain {
				fmt.Fprintln(f.Output, result)
			} else {
				modal.clear()
				modal.generation = f.Runtime.Generation()
				modal.window = &Window{Title: "Command result", Text: result, Markdown: true}
			}
			continue
		}
		if text == "/jobs" || text == "/timers" {
			var snapshot any = f.Runtime.LiveTimers()
			if text == "/jobs" {
				snapshot = f.Runtime.Jobs.Metadata("main")
			}
			body, err := json.MarshalIndent(snapshot, "", "  ")
			if err != nil {
				add("Inspection failed: "+err.Error(), 0)
			} else if f.Plain {
				fmt.Fprintln(f.Output, string(body))
			} else {
				modal.clear()
				modal.generation = f.Runtime.Generation()
				modal.window = &Window{Title: strings.TrimPrefix(text, "/"), Text: render.Fence(string(body), "json"), Markdown: true}
			}
			continue
		}
		if text == "/btw" || strings.HasPrefix(text, "/btw ") {
			if commandBusy {
				add("Error: wait for the current history command before /btw", 0)
			} else if _, err := f.Runtime.StartBTW(strings.TrimSpace(strings.TrimPrefix(text, "/btw"))); err != nil {
				add("Error: "+err.Error(), 0)
			}
			continue
		}
		if strings.HasPrefix(text, "/answer ") {
			fields := strings.SplitN(strings.TrimPrefix(text, "/answer "), " ", 2)
			if len(fields) != 2 {
				add("Error: /answer FORM_ID JSON_ARRAY", 0)
				continue
			}
			var answers []session.Answer
			if e = json.Unmarshal([]byte(fields[1]), &answers); e == nil {
				e = f.Runtime.AnswerQuestion(fields[0], answers)
			}
			if e != nil {
				add("Error: "+e.Error(), 0)
			}
			continue
		}
		if strings.HasPrefix(text, "/inspect ") {
			id, e := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(text, "/inspect ")), 10, 64)
			if e != nil {
				add("Error: invalid entry ID", 0)
			} else {
				inspect(id)
			}
			continue
		}
		if text == "/questions" || strings.HasPrefix(text, "/questions ") {
			openQuestions(strings.TrimSpace(strings.TrimPrefix(text, "/questions")))
			continue
		}
		if strings.HasPrefix(text, "/attach ") {
			attach(strings.TrimSpace(strings.TrimPrefix(text, "/attach ")))
			continue
		}
		if text == "/models" || text == "/model" {
			openModelMenu()
			continue
		}
		if strings.HasPrefix(text, "/model ") {
			fields := strings.Fields(text)
			if len(fields) < 2 || len(fields) > 3 {
				add("Error: /model ID [VARIANT]", 0)
				continue
			}
			variant := ""
			if len(fields) == 3 {
				variant = fields[2]
			}
			selectModel(fields[1], variant)
			continue
		}
		if text == "/login" {
			if busy {
				add("Login requires an idle turn", 0)
				continue
			}
			if screen != nil {
				add("Run ttc --login for device authorization", 0)
				continue
			}
			if f.Login == nil {
				add("Login requires plain mode: ttc --login", 0)
				continue
			}
			if e = f.Runtime.Provider.Login(ctx, f.Login); e != nil {
				add("Error: "+e.Error(), 0)
			}
			continue
		}
		if text == "/sessions" && !f.Plain {
			openSessionMenu()
			continue
		}
		if text == "/history" && !f.Plain {
			openHistoryMenu()
			continue
		}
		if text == "/background" {
			if commandBusy {
				add("Background requires the current history command to finish", 0)
				continue
			}
			if f.Plain {
				for _, job := range f.Runtime.Jobs.Foreground("main") {
					if _, err := f.Runtime.Jobs.Promote("main", job.ID); err != nil {
						add("Background failed: "+err.Error(), 0)
					}
				}
			} else {
				openBackgroundMenu()
			}
			continue
		}
		if strings.HasPrefix(text, "/") {
			if busy {
				add("Command requires an idle turn; Esc twice interrupts work, Ctrl+C exits", 0)
				continue
			}
			busy = true
			commandBusy = true
			started = time.Now()
			// Lifecycle commands join callbacks; keep draining their UI events while they wait.
			go func() {
				result, err := f.Runtime.Command(text)
				markdown := text == "/compact" || strings.HasPrefix(text, "/compact ")
				if json.Valid([]byte(result)) {
					result = render.Fence(result, "json")
					markdown = true
				}
				done <- operationResult{text: result, command: strings.Fields(text)[0], markdown: markdown, err: err}
			}()
			continue
		}
		pending := contextbuild.Input{Text: submit, Attachments: attachments}
		if redirected, err := f.Runtime.RedirectDismissedQuestion(pending); err != nil {
			draft.set(submit)
			add("Redirect failed: "+err.Error(), 0)
			continue
		} else if redirected {
			attachments = nil
			continue
		}
		if steering {
			if err := f.Runtime.Steer(pending); err != nil {
				draft.set(submit)
				add("Steering failed: "+err.Error(), 0)
			} else {
				attachments = nil
			}
			continue
		}
		attachments = nil
		pending.Source = "normal"
		if busy || len(queue) > 0 {
			pending.Source = "queue"
		}
		queue = append(queue, pending)
		if busy {
			add(fmt.Sprintf("Queued input · %d unsent", len(queue)), 0)
		}
	}
}
func put(s tcell.Screen, x, y, width int, text string, style tcell.Style) {
	for _, r := range text {
		if width <= 0 {
			return
		}
		cells := runewidth.RuneWidth(r)
		if cells < 1 {
			cells = 1
		}
		if cells > width {
			return
		}
		s.SetContent(x, y, r, nil, style)
		x += cells
		width -= cells
	}
}
func draw(s tcell.Screen, view *transcript, sidebar *sidebar, fullscreen bool, preview *imagePreview, renderer *imageRenderer, focused int, draft composer, attached int, queue []contextbuild.Input, steers []string, indicator string, window *Window, selection provider.Selection) error {
	s.Clear()
	w, h := s.Size()
	paneWidth := sidebar.bounds(w, h, fullscreen)
	w = paneWidth
	style := tcell.StyleDefault.Foreground(tcell.GetColor(render.TextColor))
	queuedLines := min(len(queue)+len(steers), max(0, h-4))
	height := max(0, h-3-queuedLines)
	if fullscreen {
		queuedLines = 0
		height = max(0, h-1)
		if draft.text != "" {
			height = max(0, h-2)
		}
	}
	columns := max(1, w-1)
	if fullscreen {
		columns = max(1, w)
	}
	all := view.viewport(columns, height)
	start := view.firstLine
	if renderer != nil {
		assets := all
		if window != nil && preview == nil && window.Markdown {
			if window.renderer != renderer {
				window.renderer = renderer
				window.cachedLines = nil
			}
			// Prepare the popup viewport before requesting assets. The shared
			// worker and memory bound cover both visible conversation and popup.
			fullWidth, fullHeight := s.Size()
			innerWidth, bodyHeight := windowContentSize(fullWidth, fullHeight, window)
			rows := window.Lines(innerWidth, bodyHeight)
			assets = append(append([]line(nil), all...), make([]line, len(rows))...)
			for i := range rows {
				row := window.assets[window.Scroll+i]
				assets[len(all)+i] = line{text: row.text, assets: row.assets}
			}
		}
		if err := renderer.ensure(assets); err != nil {
			return err
		}
	}
	for y, i := 0, 0; y < height && i < len(all); y, i = y+1, i+1 {
		st := style
		if all[i].human {
			st = st.Background(tcell.GetColor(render.HumanColor))
			put(s, 0, y, columns, strings.Repeat(" ", columns), st)
		}
		if all[i].system {
			st = st.Foreground(tcell.GetColor(render.LavenderColor))
		}
		if focused >= 0 && focused < len(view.lines) && all[i].id != 0 && all[i].id == view.lines[focused].id {
			st = st.Reverse(true)
		}
		if strings.ContainsRune(all[i].text, '\U0010eeee') {
			st = st.Reverse(false)
		}
		if all[i].markdown || all[i].styled {
			putStyled(s, 0, y, columns, all[i].text, st)
		} else {
			put(s, 0, y, columns, all[i].text, st)
		}
	}
	inputText, inputCursor := draft.viewport(max(0, w-2))
	if !fullscreen {
		drawScrollBar(s, w-1, 0, height, view.total, start, style.Foreground(tcell.GetColor(render.MutedColor)))
	}
	status := selection.Model.ID + " · " + selection.Variant + " · /help · Ctrl+X F full · Ctrl+X M model"
	if indicator != "" {
		status = indicator
	}
	if !fullscreen {
		put(s, 0, height, w, status, style.Foreground(tcell.GetColor(render.CyanColor)))
		for i := range queuedLines {
			label := "Queued · "
			var source string
			if i < len(steers) {
				label = "Steer · "
				source = steers[i]
			} else {
				source = queue[i-len(steers)].Text
			}
			text := pendingInputPreview(label, source, w)
			put(s, 0, height+1+i, w, strings.Repeat(" ", max(0, w)), style.Background(tcell.GetColor(render.HumanColor)))
			putStyled(s, 0, height+1+i, w, text, style.Background(tcell.GetColor(render.HumanColor)))
		}
	}
	if !fullscreen || draft.text != "" {
		put(s, 0, h-2, w, "> ", style)
		putStyled(s, 2, h-2, max(0, w-2), inputText, style)
	}
	progress := scrollIndicator(view.total, height, start, w)
	if len(view.blocks) > len(view.recent) {
		progress = "~" + progress
	}
	if fullscreen {
		progress = ""
		put(s, 0, h-1, w, "Copy mode · updates paused · Esc returns", style.Foreground(tcell.GetColor(render.CyanColor)))
	}
	if attached > 0 {
		put(s, 0, h-1, max(0, w-runewidth.StringWidth(progress)-1), fmt.Sprintf("%d attachments", attached), style)
	}
	put(s, max(0, w-runewidth.StringWidth(progress)), h-1, w, progress, style.Foreground(tcell.GetColor(render.MutedColor)))
	sidebar.draw(s)
	if window == nil && preview == nil && draft.completion != nil {
		draft.completion.draw(s, h-2)
	}
	if preview != nil && renderer != nil {
		s.HideCursor()
		if err := preview.draw(s, renderer.graphics); err != nil {
			return err
		}
	} else if window != nil {
		s.HideCursor()
		drawWindow(s, window)
	} else if w > 0 && h >= 2 && (!fullscreen || draft.text != "") {
		s.ShowCursor(min(2+inputCursor, w-1), h-2)
	} else {
		s.HideCursor()
	}
	s.Show()
	return nil
}

func drawScrollBar(s tcell.Screen, x, top, height, total, offset int, style tcell.Style) {
	if x < 0 || height <= 0 {
		return
	}
	thumb := height
	position := 0
	if total > height {
		thumb = max(1, height*height/total)
		position = min(max(0, offset), total-height) * (height - thumb) / (total - height)
	}
	for y := range height {
		r := '│'
		if y >= position && y < position+thumb {
			r = '█'
		}
		s.SetContent(x, top+y, r, nil, style)
	}
}

func drawWindowFrame(s tcell.Screen, window *Window) {
	w, h := s.Size()
	left, top, width, height := windowBounds(w, h)
	if width < 2 || height < 2 {
		return
	}
	style := tcell.StyleDefault.Background(tcell.GetColor(render.SurfaceColor)).Foreground(tcell.GetColor(render.TextColor))
	if window.System {
		style = style.Foreground(tcell.GetColor(render.LavenderColor))
	}
	for y := range height {
		put(s, left, top+y, width, strings.Repeat(" ", width), style)
	}
	for x := 1; x < width-1; x++ {
		s.SetContent(left+x, top, '─', nil, style)
		s.SetContent(left+x, top+height-1, '─', nil, style)
	}
	for y := 1; y < height-1; y++ {
		s.SetContent(left, top+y, '│', nil, style)
		s.SetContent(left+width-1, top+y, '│', nil, style)
	}
	s.SetContent(left, top, '┌', nil, style)
	s.SetContent(left+width-1, top, '┐', nil, style)
	s.SetContent(left, top+height-1, '└', nil, style)
	s.SetContent(left+width-1, top+height-1, '┘', nil, style)
	title := render.Clean(window.Title)
	if !window.HideHint {
		hint := window.Hint
		if hint == "" {
			hint = "Esc closes"
		}
		title += render.Clean(" · " + hint)
	}
	if window.subagentName != "" {
		title = render.SubagentBadge(window.actor, window.subagentName, max(1, (width-2)/2), true) + " " + title
	}
	putStyled(s, left+1, top, width-2, title, style.Bold(true))
}
func drawWindow(s tcell.Screen, window *Window) {
	w, h := s.Size()
	left, top, width, height := windowBounds(w, h)
	if width < 2 || height < 2 {
		return
	}
	style := tcell.StyleDefault.Background(tcell.GetColor(render.SurfaceColor)).Foreground(tcell.GetColor(render.TextColor))
	if window.System {
		style = style.Foreground(tcell.GetColor(render.LavenderColor))
	}
	drawWindowFrame(s, window)
	innerWidth, bodyHeight := windowContentSize(w, h, window)
	header := window.HeaderLines(innerWidth, max(0, height-3))
	view := window.Lines(innerWidth, bodyHeight)
	for i, text := range append(header, view...) {
		if (window.Markdown || window.Styled) && i >= len(header) {
			putStyled(s, left+1, top+1+i, innerWidth, text, style)
		} else {
			put(s, left+1, top+1+i, innerWidth, text, style)
		}
	}
	progress := window.Progress(bodyHeight, width-2)
	put(s, left+width-1-runewidth.StringWidth(progress), top+height-1, width-2, progress, style.Bold(true))
	drawScrollBar(s, left+width-1, top+1+len(header), bodyHeight, len(window.cachedLines), window.Scroll, style)
}

// windowContentSize accounts for the border and fixed headers in every caller.
func windowContentSize(w, h int, window *Window) (width, height int) {
	_, _, outerWidth, outerHeight := windowBounds(w, h)
	width, height = max(0, outerWidth-2), max(0, outerHeight-2)
	return width, height - len(window.HeaderLines(width, max(0, height-1)))
}

func windowBounds(w, h int) (left, top, width, height int) {
	left, top, width, height = 2, 1, w-4, h-2
	if width < 5 || height < 3 {
		left, top, width, height = 0, 0, w, h
	}
	return
}

func waitForTurn(done <-chan operationResult, events <-chan session.Event) {
	for {
		select {
		case <-done:
			return
		case <-events:
		}
	}
}
