package tui

import (
	"fmt"
	"strings"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/render"
	"ttc/internal/session"

	"github.com/gdamore/tcell/v3"
)

func (c *frontendController) start(input *session.InputAdmission) {
	if input != nil {
		c.notificationPaused = false
	}
	c.busy = true
	c.started = time.Now()
	c.activity = turnActivity{}
	c.pendingInput = input
	runtime, done := c.frontend.Runtime, c.done
	go func() {
		if input == nil {
			done <- operationResult{err: runtime.Run(nil)}
		} else {
			done <- operationResult{err: runtime.RunInput(input)}
		}
	}()
}

func (c *frontendController) prepare() (bool, error) {
	if generation := c.frontend.Runtime.Generation(); generation != c.queueGeneration {
		if c.cancelAttachment != nil {
			// Keep ownership until the event loop drains completion; a replacement
			// cannot race the canceled worker or its shutdown join.
			c.cancelAttachment()
		}
		c.queue = nil
		c.pendingInput = nil
		c.notificationPaused = false
		c.queueGeneration = generation
		c.activity = turnActivity{}
		c.retainedQuestion = nil
		c.deferredQuestionID = ""
		c.questionRow = -1
		if c.modal.question != nil {
			c.modal.clear()
		}
	}
	c.syncQuestionState(c.frontend.Runtime.PendingQuestion())
	if !c.frontend.Plain && !c.fullscreen && !c.editing && !c.draft.pasting && c.modal.empty() && len(c.pendingBTW) > 0 {
		event := c.pendingBTW[0]
		c.pendingBTW = c.pendingBTW[1:]
		if event.Generation == c.frontend.Runtime.Generation() {
			c.modal.generation = event.Generation
			c.modal.window = &Window{Title: "/btw · read-only answer", Text: event.Text, Markdown: true}
		}
	}
	if c.deferredQuestionID != "" && !c.fullscreen && !c.editing && !c.draft.pasting && c.modal.empty() {
		id := c.deferredQuestionID
		c.deferredQuestionID = ""
		if form := c.frontend.Runtime.PendingQuestion(); form != nil && form.ID == id && !form.Dismissed {
			c.showQuestion(form)
		}
	}
	if !c.busy {
		if event, err := c.frontend.Runtime.ApplyModel(""); err != nil {
			return false, err
		} else if event.Kind != "" {
			c.notificationPaused = false
			c.add(event.Text, event.EntryID)
			c.view.lines[len(c.view.lines)-1].system = true
		}
		if !c.notificationPaused && c.frontend.Runtime.HasNotifications() {
			c.start(nil)
		} else if len(c.queue) > 0 {
			input := c.frontend.Runtime.PrepareInput(c.queue[0])
			c.queue[0] = contextbuild.Input{}
			c.queue = c.queue[1:]
			c.start(input)
		} else if c.eof {
			return true, nil
		}
	}
	c.draft.completion = nil
	if c.screen != nil && c.modal.empty() && !c.fullscreen && !c.editing {
		q, active := completionAt(c.draft, c.frontend.Runtime.Workspace.Root, c.frontend.Runtime.Generation())
		if active && !(c.completion.hasDismissed && q == c.completion.dismissed) {
			if !c.hadQuery || q != c.lastQuery {
				c.lastQuery, c.hadQuery = q, true
				c.completion.selected = 0
				c.completion.result = completionResult{query: q}
				if q.marker == '/' {
					c.completion.result = commandCompletions(q)
				} else {
					c.completer.query(q)
				}
			}
			c.draft.completion = c.completion
		} else {
			c.hadQuery = false
		}
	}
	return false, nil
}

func (c *frontendController) handleRenderResult(result renderReply) {
	if result.source {
		if c.modal.loading != nil && c.modal.loading.snapshot.ID == result.key && c.modal.loading.generation == c.frontend.Runtime.Generation() {
			if result.err != nil {
				c.modal.window.Text = "Image preview failed: " + result.err.Error()
			} else {
				c.modal.preview = newImagePreview(c.modal.loading.snapshot, result.pixels, c.frontend.Runtime.ImageClickPending(result.key), c.renderer.cellWidth, c.renderer.cellHeight)
				c.attributeWindow(&c.modal.preview.Window, c.modal.loading.snapshot.Actor)
				inspection, err := c.history.ReadInspection(c.ctx, c.modal.loading.entryID)
				c.modal.preview.detailText = inspection.Text
				if err != nil {
					c.modal.preview.detailText = "Inspection failed: " + err.Error()
				}
				c.modal.window = &c.modal.preview.Window
			}
			c.modal.loading = nil
		}
	} else {
		if c.renderer.accept(result) {
			c.view.invalidate()
			failure := result.err
			if result.backend == "" {
				failure = c.renderer.ready[result.key].err
			}
			if failure != nil {
				c.view.append(line{text: "Warning: math/image rendering failed: " + render.Clean(failure.Error()), system: true})
			}
		}
	}
}

func (c *frontendController) handleAttachmentResult(result attachmentResult) {
	c.attaching = false
	c.cancelAttachment()
	c.cancelAttachment = nil
	if result.generation == c.frontend.Runtime.Generation() {
		if result.err != nil {
			c.add("Attachment failed: "+result.err.Error(), 0)
		} else {
			c.attachments = append(c.attachments, result.attachment)
			c.add("Attached · "+result.attachment.Path, 0)
		}
	}
}

func (c *frontendController) handleEditorResult(result operationResult) error {
	c.editing = false
	if err := c.screen.Resume(); err != nil {
		return fmt.Errorf("resume terminal after editor: %w", err)
	}
	c.editorSuspended = false
	c.screen.EnablePaste()
	c.screen.SetCursorStyle(tcell.CursorStyleBlinkingBar)
	if c.fullscreen {
		c.screen.DisableMouse()
	} else {
		c.screen.EnableMouse(tcell.MouseButtonEvents)
	}
	c.screen.Sync()
	c.dirty = true
	c.applyEditorResult(result)
	c.applyEditorResult = nil
	return nil
}

func (c *frontendController) handleOperationResult(result operationResult) {
	c.activity = turnActivity{}
	if c.modal.generation != 0 && c.modal.generation != c.frontend.Runtime.Generation() {
		c.modal.clear()
	}
	c.busy = false
	c.commandBusy = false
	if result.err != nil {
		c.add("Error: "+result.err.Error(), 0)
		if result.command == "" || result.command == "/compact" {
			c.notificationPaused = true
			c.add("Auto-wake paused after failure/interruption; pending notifications remain queued. Send a prompt, successfully /compact or /load, or change session/model to resume.", 0)
		}
	} else if result.command != "" {
		switch result.command {
		case "/compact", "/load", "/new":
			c.notificationPaused = false
		}
		c.replay()
		if c.frontend.Plain {
			c.add(result.text, 0)
		} else {
			switch result.command {
			case "/jobs", "/timers", "/compact":
				if c.modal.empty() {
					c.modal.window = &Window{Title: "Command result", Text: result.text, Markdown: result.markdown}
				}
			case "/load", "/branch":
				// The loaded conversation is the success feedback.
			default:
				c.add(result.text, 0)
			}
		}
	}
	c.focused = len(c.view.lines) - 1
}

func (c *frontendController) handleRuntimeEvent(event session.Event) error {
	if event.Generation != c.frontend.Runtime.Generation() || event.Kind != "btw_result" && event.SessionID != "" && event.SessionID != c.frontend.Runtime.Current() {
		return nil
	}
	c.activity.observe(event, time.Now())
	switch event.Kind {
	case "continuation":
		// Rebuild the committed history before rendering subsequent coding
		// events. Compaction keeps drafts, queued input and live dialogs.
		following, firstLine := c.view.followTail, c.view.firstLine
		c.replay()
		if !following {
			c.view.followTail, c.view.firstLine = false, firstLine
		}
		if c.frontend.Plain {
			fmt.Fprintln(c.frontend.Output, render.Clean(event.Text))
		}
	case "btw_result":
		if c.frontend.Plain {
			fmt.Fprintln(c.frontend.Output, render.Clean(event.Text))
		} else {
			// Older answers remain inspectable in history if the user keeps
			// a dialog open while launching many independent asides.
			if len(c.pendingBTW) == 16 {
				c.pendingBTW = c.pendingBTW[1:]
			}
			c.pendingBTW = append(c.pendingBTW, event)
		}
	case "usage":
		return nil
	case "session_name":
		saved, err := c.frontend.Runtime.CurrentSession()
		if err != nil {
			c.add("Session metadata failed: "+err.Error(), 0)
		} else {
			c.sidebar.sessionName = saved.Name
		}
		return nil
	case "tool_pending":
		c.addActor(line{text: event.Text, id: event.EntryID, callID: event.CallID, awaiting: true, system: true}, event.Actor)
	case "tool_progress":
		if index, ok := c.view.calls[event.CallID]; ok && c.view.lines[index].awaiting {
			item := c.view.lines[index]
			item.text = event.Text
			c.view.replace(index, item)
		}
	case "tool_stream_end":
		for key, index := range c.view.calls {
			if strings.HasPrefix(key, event.PendingKey) && c.view.lines[index].awaiting {
				item := c.view.lines[index]
				item.awaiting = false
				item.text = strings.Replace(item.text, "awaiting ", "Tool "+event.Text+" · ", 1)
				c.view.replace(index, item)
			}
		}
	case "image":
		if c.renderer != nil && event.Image != nil {
			if index, ok := c.view.calls[event.CallID]; ok {
				item := c.view.lines[index]
				item.image = event.Image
				c.view.replace(index, item)
			} else {
				c.view.append(c.attribute(line{text: "**image_show** · " + render.Inline(event.Image.Path), callID: event.CallID, markdown: true, brief: true, image: event.Image}, event.Actor))
			}
		}
	case "tool_update", "tool":
		item := c.attribute(line{text: event.Text, id: event.EntryID, callID: event.CallID, jobID: event.JobID, detail: event.Detail, markdown: true, brief: true, complete: event.Kind == "tool"}, event.Actor)
		if !c.view.publish(item, event.PendingKey, false) {
			break
		}
		if c.frontend.Plain {
			brief, err := render.TerminalBriefing(event.Text, 500, false)
			if err != nil {
				return fmt.Errorf("render tool briefing: %w", err)
			}
			fmt.Fprintln(c.frontend.Output, c.plainActor(event.Actor, brief))
		}
		if c.modal.window != nil && c.modal.window.CallID != "" && (c.modal.window.CallID == event.CallID || c.modal.window.CallID == event.PendingKey) {
			c.modal.window.CallID = event.CallID
			if event.Kind == "tool" {
				c.inspect(event.EntryID)
			} else {
				c.modal.window.JobID, c.modal.window.Detail, c.modal.window.Text = event.JobID, event.Detail, event.Detail
				if c.modal.window.JobID != "" && !c.commandBusy {
					c.modal.window.Text = c.frontend.Runtime.JobDetail(c.modal.window.JobID, c.modal.window.Detail)
				}
			}
		}
	case "job":
		c.view.append(c.attribute(line{text: event.Text, id: event.EntryID, markdown: true, brief: true}, event.Actor))
		if c.frontend.Plain {
			brief, err := render.TerminalBriefing(event.Text, 500, false)
			if err != nil {
				return fmt.Errorf("render job briefing: %w", err)
			}
			fmt.Fprintln(c.frontend.Output, c.plainActor(event.Actor, brief))
		}
	case "delta":
		item := messageLine(line{}, llm.Message{Type: llm.TypeAssistant, Content: event.Text, RequestID: event.RequestID})
		item.complete, item.markdown = false, false
		item = c.attribute(item, event.Actor)
		if item.compactResponse {
			item.id = -event.RequestID // Negative UI-only identity until the saved response arrives.
		}
		if c.view.publish(item, "", true) && c.frontend.Plain {
			if !item.compactResponse {
				fmt.Fprint(c.frontend.Output, event.Text)
			}
		}
		if c.modal.window != nil && c.modal.window.requestID == event.RequestID {
			c.modal.window.Text = c.view.lines[c.view.requests[event.RequestID]].text
		}
	case "assistant":
		item := c.attribute(messageLine(line{id: event.EntryID}, llm.Message{Type: llm.TypeAssistant, Content: event.Text, RequestID: event.RequestID}), event.Actor)
		index, streamed := c.view.requests[event.RequestID]
		streamed = streamed && !c.view.lines[index].complete
		if c.view.publish(item, "", false) && c.frontend.Plain {
			if item.compactResponse {
				fmt.Fprintln(c.frontend.Output, render.SubagentResponse(event.Actor, c.nameForActor(event.Actor), item.speaker, event.Text, 500, false))
			} else if streamed {
				fmt.Fprintln(c.frontend.Output)
			} else {
				fmt.Fprintln(c.frontend.Output, c.plainActor(event.Actor, item.speaker+" · "+event.Text))
			}
		}
		if c.graphicsWarning != "" {
			hasMath := false
			render.Math(event.Text, func(tex string, block bool) string { hasMath = true; return tex })
			if hasMath {
				c.view.append(line{text: "Warning: " + c.graphicsWarning, system: true})
				c.graphicsWarning = ""
			}
		}
		if c.modal.window != nil && c.modal.window.requestID == event.RequestID {
			c.inspect(event.EntryID)
		}
	case "question":
		if event.Question == nil {
			return fmt.Errorf("question event is missing its form")
		}
		form := c.frontend.Runtime.PendingQuestion()
		c.syncQuestionState(form)
		if form == nil || form.ID != event.Question.ID {
			break // Ignore stale publication; tool intents remain inspectable.
		}
		if c.frontend.Plain {
			fmt.Fprintln(c.frontend.Output, render.Clean(event.Text))
		}
		if !c.frontend.Plain && !form.Dismissed && c.modal.question == nil {
			if !c.modal.empty() || c.draft.pasting || c.fullscreen || c.editing {
				c.deferredQuestionID = form.ID
			} else {
				c.showQuestion(form)
			}
		}
	case "question_closed":
		// Runtime state may already contain the next round. A delayed
		// closure must not clear that round's activity or dialog.
		c.syncQuestionState(c.frontend.Runtime.PendingQuestion())
	case "system_prompt":
		if c.showPrompt(event.EntryID) {
			c.addActor(line{text: event.Text, id: event.EntryID, system: true}, event.Actor)
		}
	case "runtime_context":
		c.addActor(messageLine(line{id: event.EntryID}, llm.Message{Type: llm.TypeRuntimeContext}), event.Actor)
	case "status", "wake":
		c.addActor(line{text: event.Text, id: event.EntryID, system: true}, event.Actor)
	default:
		item := line{text: event.Text, id: event.EntryID, actor: event.Actor}
		if event.Kind == "message" {
			item = messageLine(item, llm.Message{Type: event.MessageType, Content: event.Text})
		}
		item = c.attribute(item, item.actor)
		c.view.append(item)
		if c.frontend.Plain {
			if item.compactResponse {
				fmt.Fprintln(c.frontend.Output, render.SubagentResponse(item.actor, item.subagentName, item.speaker, item.text, 500, false))
			} else {
				fmt.Fprintln(c.frontend.Output, c.plainActor(item.actor, item.text))
			}
		}
	}
	c.focused = len(c.view.lines) - 1
	return nil
}

const (
	frontendBatchEvents = 2048
	frontendBatchTime   = 4 * time.Millisecond
)

// receive waits for one event, then coalesces ready terminal/runtime work only
// during bracketed paste. Ordinary keys retain a prepare/draw between events
// (notably completion and modal updates). Other sources return to the full
// select after at most one bounded batch, even under continuous paste input.
func (c *frontendController) receive() (next frontendSubmission, stop bool, err error) {
	c.batchingEvents = true
	defer func() {
		c.flushPaste()
		c.batchingEvents = false
	}()
	next, stop, err = c.receiveOne()
	if stop || err != nil || next.ready {
		return next, stop, err
	}
	if !c.paste.active {
		return next, false, nil
	}
	// A short bounded window also coalesces decoder bursts separated by tiny
	// scheduling gaps, rather than drawing once per drained terminal buffer.
	deadline := time.Now().Add(frontendBatchTime)
	timer := time.NewTimer(frontendBatchTime)
	defer timer.Stop()
	for count := 1; c.paste.active && count < frontendBatchEvents && time.Now().Before(deadline); count++ {
		select {
		case <-c.ctx.Done():
			return c.receiveCanceled()
		case ev, ok := <-c.keys:
			next = c.receiveScreenEvent(ev, ok)
		case event := <-c.frontend.Events:
			c.flushPaste()
			err = c.handleRuntimeEvent(event)
		case <-timer.C:
			return next, false, nil
		}
		if err != nil || next.ready {
			return next, false, err
		}
	}
	return next, false, nil
}

func (c *frontendController) receiveCanceled() (frontendSubmission, bool, error) {
	c.frontend.Runtime.Interrupt()
	if c.busy {
		waitForTurn(c.done, c.frontend.Events)
		c.busy = false
	}
	return frontendSubmission{}, true, c.ctx.Err()
}

func (c *frontendController) receiveScreenEvent(ev tcell.Event, ok bool) frontendSubmission {
	if !ok {
		c.keys = nil
		return frontendSubmission{}
	}
	if c.editing {
		return frontendSubmission{}
	}
	c.dirty = true
	return c.handleScreenEvent(ev)
}

func (c *frontendController) receiveOne() (next frontendSubmission, stop bool, err error) {
	select {
	case <-c.ctx.Done():
		return c.receiveCanceled()
	case _, ok := <-c.graphicsActivation:
		if !ok {
			c.graphicsActivation = nil
			c.add("Warning: tmux image recovery observer stopped.", 0)
			return next, false, nil
		}
		c.g.Refresh()
		if !c.editorSuspended {
			c.screen.Sync()
			c.dirty = true
		}
	case update, ok := <-c.modelUpdates:
		if !ok {
			c.modelUpdates = nil
			return next, false, nil
		}
		if update.Err != nil {
			c.add("Warning: model catalog refresh failed; retaining cached catalog: "+update.Err.Error(), 0)
		} else {
			// Replace rather than mutate: an open menu owns the old snapshot.
			// Runtime selection (including in-flight metadata) changes only on
			// an explicit selectModel request, never on refresh delivery.
			c.frontend.Models = update.Models
		}
	case result := <-c.renderResults:
		c.handleRenderResult(result)
	case result := <-c.completer.results:
		q, active := completionAt(c.draft, c.frontend.Runtime.Workspace.Root, c.frontend.Runtime.Generation())
		if active && result.query == q && c.modal.empty() && !c.fullscreen {
			c.completion.result, c.completion.selected = result, 0
			if result.err != nil {
				c.completion.result.items = nil
			}
		}
	case result := <-c.attachmentDone:
		c.handleAttachmentResult(result)
	case result := <-c.editorDone:
		if err := c.handleEditorResult(result); err != nil {
			return next, false, err
		}
	case result := <-c.done:
		c.handleOperationResult(result)
	case event := <-c.frontend.Events:
		// Aside results belong to the live generation, which survives
		// compaction. Explicit session changes still discard old popups.
		if err := c.handleRuntimeEvent(event); err != nil {
			return next, false, err
		}
	case v := <-c.inputs:
		next.text = v.text
		next.ready = true
	case ev, ok := <-c.keys:
		return c.receiveScreenEvent(ev, ok), false, nil
	case v := <-c.workspaceUpdates:
		c.sidebar.workspace = v
	case <-c.ticker.C:
	}
	return next, false, nil
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
