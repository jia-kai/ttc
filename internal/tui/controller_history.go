package tui

import (
	"fmt"
	"strings"
	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/messagestream"
	"ttc/internal/render"
)

func (c *frontendController) add(text string, id int64) {
	c.view.append(line{text: text, id: id})
	if c.frontend.Plain {
		fmt.Fprintln(c.frontend.Output, render.Clean(text))
	}
}

func (c *frontendController) nameForActor(actor string) string {
	if actor == "" || actor == "main" {
		return ""
	}
	if name, found := c.names[actor]; found {
		return name
	}
	loaded, err := c.history.SubagentNames(c.ctx, c.frontend.Runtime.Current())
	if err != nil {
		c.add("Subagent names failed: "+err.Error(), 0)
	}
	for id, name := range loaded {
		c.names[id] = name
	}
	name := c.names[actor]
	c.names[actor] = name // Cache unnamed asides too; never query per chunk or frame.
	return name
}

func (c *frontendController) attribute(item line, actor string) line {
	item.actor, item.subagentName = actor, c.nameForActor(actor)
	item.compactResponse = item.speaker != "" && (strings.HasPrefix(actor, "main/child_") || item.subagentName != "")
	return item
}

func (c *frontendController) attributeWindow(window *Window, actor string) {
	window.actor, window.subagentName = actor, c.nameForActor(actor)
}

func (c *frontendController) sourceWindow(window *Window, actor string) {
	c.attributeWindow(window, actor)
	name := window.subagentName
	if actor == "" || actor == "main" {
		name = "Main agent"
	} else if name == "" {
		name = actor
	}
	window.SourceAgent = name
}

func (c *frontendController) plainActor(actor, text string) string {
	if name := c.nameForActor(actor); name != "" {
		return render.SubagentBadge(actor, name, 64, false) + " " + render.Clean(text)
	}
	return render.Clean(text)
}

func (c *frontendController) addActor(item line, actor string) {
	c.view.append(c.attribute(item, actor))
	if c.frontend.Plain {
		text := item.text
		if item.brief && !item.markdown {
			text = render.ResponsePrefix(text)
		}
		fmt.Fprintln(c.frontend.Output, c.plainActor(actor, text))
	}
}

// inspectionShowsSource applies the same provenance policy to small and paged records.
func inspectionShowsSource(kind, eventType string) bool {
	return kind == "tool_call" || kind == "tool_result" || eventType == "job_completion" || eventType == "presentation_stream"
}

func (c *frontendController) inspectSaved(id int64) {
	c.esc, c.viewChord = false, false
	historyView := c.modal.history
	c.modal.clear()
	c.modal.history = historyView
	c.modal.generation = c.frontend.Runtime.Generation()
	if id <= 0 {
		c.add("Inspection failed: entry ID must be positive", 0)
		return
	}
	if !c.frontend.Plain {
		page, err := c.history.InspectPage(c.ctx, id, 0, history.InspectionPageChars)
		if err != nil {
			c.add("Inspection failed: "+err.Error(), 0)
			return
		}
		if page.Supported && (page.Total > page.Limit || page.LargeEnvelope) {
			c.modal.window = NewPagedWindow(page, func(offset int) (history.InspectionPage, error) {
				return c.history.InspectPage(c.ctx, id, offset, history.InspectionPageChars)
			})
			if inspectionShowsSource(page.Kind, page.Type) {
				c.sourceWindow(c.modal.window, page.Actor)
			} else {
				c.attributeWindow(c.modal.window, page.Actor)
			}
			return
		}
	}
	if c.renderer != nil {
		snapshot, err := c.frontend.Runtime.ImageForEntry(id)
		if err == nil && snapshot != nil {
			if err := c.renderer.loadSource(*snapshot); err != nil {
				c.add("Image preview failed: "+err.Error(), 0)
				return
			}
			c.modal.loading = &previewLoad{snapshot: *snapshot, generation: c.frontend.Runtime.Generation(), entryID: id}
			c.modal.window = &Window{Title: "Image", Text: "Loading full image…"}
			c.attributeWindow(c.modal.window, snapshot.Actor)
			return
		}
	}
	inspection, err := c.history.ReadInspection(c.ctx, id)
	if err != nil {
		c.add("Error: "+err.Error(), 0)
		return
	}
	if c.frontend.Plain {
		fmt.Fprintln(c.frontend.Output, render.Clean(inspection.Text))
	} else {
		c.modal.window = &Window{Title: inspection.Title, Text: inspection.Text, System: inspection.System, Markdown: inspection.Markdown, Structured: inspection.Structured}
		if inspectionShowsSource(inspection.Kind, inspection.Type) {
			c.sourceWindow(c.modal.window, inspection.Actor)
		} else {
			c.attributeWindow(c.modal.window, inspection.Actor)
		}
		if c.modal.window.Markdown {
			title, err := render.TerminalBriefing(c.modal.window.Title, 512, false)
			if err != nil {
				c.add("Inspection failed: "+err.Error(), 0)
				c.modal.clear()
				return
			}
			c.modal.window.Title = title
		}
		for _, item := range c.view.lines {
			if item.id == id && item.callID != "" && !item.complete {
				if strings.HasPrefix(item.callID, "stream:") {
					c.modal.window.CallID = item.callID
					c.modal.window.Markdown, c.modal.window.System = true, false
					break
				}
				c.modal.window.Markdown, c.modal.window.System = true, false
				c.modal.window.CallID, c.modal.window.JobID, c.modal.window.Detail, c.modal.window.Text = item.callID, item.jobID, item.detail, item.detail
				if item.jobID != "" && !c.commandBusy {
					c.modal.window.Text = c.frontend.Runtime.JobDetail(item.jobID, item.detail)
				}
				break
			}
		}
	}
}

func (c *frontendController) inspect(id int64) {
	if id <= 0 {
		c.inspectSaved(id)
		return
	}
	response, err := c.history.ChildResponse(id)
	if err != nil {
		c.add("Child response inspection failed: "+err.Error(), 0)
		return
	}
	if response == 0 {
		c.inspectSaved(id)
		return
	}
	c.inspectSaved(response)
	if c.modal.window != nil {
		c.modal.window.completionEntry = id
		c.modal.window.Hint = "D completion details · Esc closes"
	}
}

// openRow dispatches only the interaction explicitly offered by a transcript row.
func (c *frontendController) openRow(item line) {
	switch item.action {
	case actionInspect:
		c.inspectSaved(item.inspectID)
		c.modal.requestHeader = item.id
		if c.modal.window != nil && item.rawID > 0 {
			c.modal.window.Title = requestWindowTitle(c.modal.window.Title)
			c.modal.window.boundaryRaw = item.rawID
			c.modal.window.Hint = "E expand · R raw · [ ] pages"
		}
	case actionExecution:
		c.inspectExecution(item.inspectID)
	case actionQuestion:
		if form := c.frontend.Runtime.PendingQuestion(); form != nil && form.ID == item.questionID {
			c.openQuestions(item.questionID)
		} else if item.inspectID != 0 {
			c.inspectSaved(item.inspectID)
		}
	}
}

func (c *frontendController) inspectExecution(id int64) {
	c.inspect(id)
	if c.modal.window != nil {
		c.modal.window.Title = "Execution details · " + c.modal.window.Title
	}
}

// refreshRequestInspection keeps an open readable request in sync as native
// response items complete. Raw captures are immutable and are not refreshed.
func (c *frontendController) refreshRequestInspection(header, target int64) {
	w := c.modal.window
	if w == nil || c.modal.requestHeader != header || c.modal.raw != nil || w.pages == nil || target <= 0 {
		return
	}
	expanded := c.modal.expanded
	page, err := c.requestInspectionPage(target, expanded, w.pages.page.Offset)
	if err != nil {
		w.Header = "Inspection refresh failed: " + render.Clean(err.Error())
		return
	}
	scroll := w.Scroll
	w.pages.load = func(offset int) (history.InspectionPage, error) {
		return c.requestInspectionPage(target, expanded, offset)
	}
	// A new revision may insert readable response text before its JSON tail.
	// Syntax hints from the old page offsets no longer identify the same source.
	w.pages.fences = map[int]inspectionFence{0: {}}
	w.pages.structured = map[int]render.StructuredState{0: {}}
	w.setPage(page)
	w.Title, w.boundaryRaw = requestWindowTitle(page.Title), target
	w.Header = ""
	w.Scroll = scroll
}

func (c *frontendController) requestInspectionPage(id int64, expanded bool, offset int) (history.InspectionPage, error) {
	if expanded {
		return c.history.BoundaryExpandedPage(c.ctx, id, offset, history.InspectionPageChars)
	}
	return c.history.InspectPage(c.ctx, id, offset, history.InspectionPageChars)
}

func (c *frontendController) toggleRequestExpansion() {
	old := c.modal.window
	if old == nil || old.boundaryRaw == 0 || c.modal.raw != nil {
		return
	}
	id, expanded := old.boundaryRaw, !c.modal.expanded
	page, err := c.requestInspectionPage(id, expanded, 0)
	if err != nil {
		old.Header = "Inspection failed: " + render.Clean(err.Error())
		return
	}
	w := NewPagedWindow(page, func(offset int) (history.InspectionPage, error) {
		return c.requestInspectionPage(id, expanded, offset)
	})
	w.Title = requestWindowTitle(page.Title)
	w.boundaryRaw = id
	mode := "expand"
	if expanded {
		mode = "compact"
	}
	w.Hint = "E " + mode + " · R raw · [ ] pages"
	c.modal.window, c.modal.expanded = w, expanded
	c.sourceWindow(w, page.Actor)
}

// The row already carries attempt/state. Keep the inspector heading short
// enough that its action hints remain visible in a narrow terminal pane.
func requestWindowTitle(title string) string {
	prefix, suffix, found := strings.Cut(title, " · attempt ")
	if !found {
		return title
	}
	attempt, _, _ := strings.Cut(suffix, " · ")
	if attempt == "1" {
		return prefix
	}
	return prefix + " · attempt " + attempt
}

func (c *frontendController) openBoundaryRaw(id int64) {
	c.modal.raw = &rawBoundaryState{header: id}
	c.navigateBoundaryRaw(0)
}

func (c *frontendController) navigateBoundaryRaw(delta int) {
	state := c.modal.raw
	if state == nil {
		return
	}
	captures, err := c.history.BoundaryCaptures(c.ctx, state.header)
	if err != nil || len(captures) == 0 {
		if err == nil {
			err = fmt.Errorf("request has no captured payloads")
		}
		c.add("Raw boundary inspection failed: "+err.Error(), 0)
		return
	}
	index := 0
	for i, capture := range captures {
		if capture.ID == state.capture {
			index = i
			break
		}
	}
	index = max(0, min(len(captures)-1, index+delta))
	id := captures[index].ID
	page, err := c.history.BoundaryRawPage(c.ctx, id, 0, history.InspectionPageChars)
	if err != nil {
		c.add("Raw boundary inspection failed: "+err.Error(), 0)
		return
	}
	c.modal.window = NewPagedWindow(page, func(offset int) (history.InspectionPage, error) {
		return c.history.BoundaryRawPage(c.ctx, id, offset, history.InspectionPageChars)
	})
	requestTitle, _, _ := strings.Cut(page.Title, " · attempt ")
	c.modal.window.Title = "Exact raw JSON · " + requestTitle + " · " + captures[index].Label
	c.modal.window.Hint = fmt.Sprintf("%d/%d · N/P capture · [ ] pages", index+1, len(captures))
	state.capture = id
	c.sourceWindow(c.modal.window, page.Actor)
}

func (c *frontendController) replay() {
	c.view.reset()
	c.streamRows = map[int64]int{}
	loaded, err := c.history.SubagentNames(c.ctx, c.frontend.Runtime.Current())
	if err != nil {
		c.add("Subagent names failed: "+err.Error(), 0)
		return
	}
	c.names = loaded
	saved, err := c.frontend.Runtime.CurrentSession()
	if err != nil {
		c.add("Session metadata failed: "+err.Error(), 0)
		return
	}
	c.sidebar.sessionName = saved.Name
	entries, e := c.frontend.Runtime.Entries()
	if e != nil {
		c.add("Error: "+e.Error(), 0)
		return
	}
	var stream []messagestream.Item
	if saved.EntryTip != 0 {
		stream, err = c.history.StreamItems(c.ctx, c.frontend.Runtime.Current())
		if err != nil {
			c.add("Stream presentation failed: "+err.Error(), 0)
			return
		}
	}
	for i, item := range stream {
		if item.ID <= 0 || i > 0 && item.ID <= stream[i-1].ID {
			c.add("Stream presentation failed: items must have increasing saved entry IDs", 0)
			return
		}
	}
	nextStream := 0
	for _, v := range entries {
		for nextStream < len(stream) && stream[nextStream].ID < v.ID {
			item := stream[nextStream]
			c.streamRows[item.ID] = len(c.view.lines)
			c.view.append(c.attribute(streamLine(item), item.Actor))
			nextStream++
		}
		if nextStream < len(stream) && stream[nextStream].ID == v.ID {
			item := stream[nextStream]
			c.streamRows[item.ID] = len(c.view.lines)
			c.view.append(c.attribute(streamLine(item), item.Actor))
			nextStream++
			continue
		}
		if v.InternalEvent() {
			continue
		}
		entryView, err := c.history.ReadView(c.ctx, v)
		if err != nil {
			c.add("History presentation failed: "+err.Error(), 0)
			return
		}
		if entryView.StatusType == "system_prompt" || entryView.StatusType == "request_message" {
			continue
		}
		item := c.attribute(line{text: strings.TrimSuffix(entryView.Label, " · inspect"), id: v.ID, system: v.Kind == "status" || v.Role == "system"}, v.Actor)
		if v.Kind == "tool_call" || v.Kind == "tool_result" || entryView.StatusType == "job_completion" {
			item.action, item.inspectID = actionExecution, v.ID
		}
		if v.Kind == "tool_result" || entryView.StatusType == "job_completion" {
			item.markdown, item.brief, item.system = true, true, false
		}
		if (v.Kind == "message" || v.Kind == "summary") && entryView.Message != nil {
			message := *entryView.Message
			if message.Type == llm.TypeRuntimeContext || message.Type == llm.TypeCompactionContext || message.Type == llm.TypeSystemInstruction || message.Type == llm.TypeDeveloperInstruction {
				continue // Synthetic model-input snapshots are visible at the boundary.
			}
			if v.Kind == "message" && message.Type == llm.TypeUserInput {
				// Original inputs take their commit time from the entry, not
				// caller-supplied message metadata.
				message.InputTimeMS = v.CreatedMS
			}
			item = directMessageLine(item, message)
			item = c.attribute(item, item.actor)
		}
		if v.Kind == "tool_result" && c.renderer != nil {
			snapshot, err := c.frontend.Runtime.ImageForEntry(v.ID)
			if err == nil {
				item.image = snapshot
			}
		}
		c.view.append(item)
	}
	for ; nextStream < len(stream); nextStream++ {
		item := stream[nextStream]
		c.streamRows[item.ID] = len(c.view.lines)
		c.view.append(c.attribute(streamLine(item), item.Actor))
	}
	if c.renderer != nil {
		cards, err := c.frontend.Runtime.PendingImages()
		if err != nil {
			c.add("Pending image display failed: "+err.Error(), 0)
		}
		for _, card := range cards {
			present := false
			for i, item := range c.view.lines {
				if item.image != nil && item.image.ID == card.Snapshot.ID {
					present = true
					break
				}
				if item.id == card.EntryID {
					item = c.attribute(line{text: "**image_show** · " + render.Inline(card.Snapshot.Path), id: card.EntryID, action: actionExecution, inspectID: card.EntryID, callID: card.Snapshot.ID, markdown: true, brief: true, image: &card.Snapshot}, card.Snapshot.Actor)
					c.view.replace(i, item)
					present = true
					break
				}
			}
			if !present {
				c.view.append(c.attribute(line{text: "**image_show** · " + render.Inline(card.Snapshot.Path), id: card.EntryID, action: actionExecution, inspectID: card.EntryID, callID: card.Snapshot.ID, markdown: true, brief: true, image: &card.Snapshot}, card.Snapshot.Actor))
			}
		}
	}
	c.focused = len(c.view.lines) - 1
	c.view.followTail = true
}
