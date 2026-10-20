package tui

import (
	"fmt"
	"strings"
	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/render"
)

func (c *frontendController) showPrompt(id int64) bool {
	view, err := c.history.ReadEntryView(c.ctx, id)
	if err != nil {
		return true // Keep the placeholder available to reveal the inspection error.
	}
	return c.showPromptMetadata(id, view.Prompt)
}

func (c *frontendController) showPromptMetadata(id int64, prompt *history.PromptMetadata) bool {
	if prompt == nil || prompt.Path == "" {
		return true
	}
	key := prompt.GroupKey()
	if previous, ok := c.seenPrompts[key]; ok {
		// One placeholder per actor/purpose, inspecting its latest snapshot.
		if id <= previous {
			return false
		}
		for i, item := range c.view.lines {
			if item.id == previous {
				item.id = id
				c.view.replace(i, item)
				c.seenPrompts[key] = id
				return false
			}
		}
	}
	c.seenPrompts[key] = id
	return true
}

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
		fmt.Fprintln(c.frontend.Output, c.plainActor(actor, item.text))
	}
}

// inspectionShowsSource applies the same provenance policy to small and paged records.
func inspectionShowsSource(kind, eventType string) bool {
	return kind == "tool_call" || kind == "tool_result" || eventType == "job_completion"
}

func (c *frontendController) inspectSaved(id int64) {
	c.esc, c.viewChord = false, false
	historyView := c.modal.history
	c.modal.clear()
	c.modal.history = historyView
	c.modal.generation = c.frontend.Runtime.Generation()
	if id < 0 {
		if index, found := c.view.requests[-id]; found {
			item := c.view.lines[index]
			c.modal.window = &Window{Title: "assistant", Text: item.text, Markdown: true, requestID: -id}
			c.attributeWindow(c.modal.window, item.actor)
		}
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
		c.modal.window = &Window{Title: inspection.Title, Text: inspection.Text, System: inspection.System, Markdown: inspection.Markdown}
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

func (c *frontendController) replay() {
	c.view.reset()
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
	c.seenPrompts = map[string]int64{}
	entries, e := c.frontend.Runtime.Entries()
	if e != nil {
		c.add("Error: "+e.Error(), 0)
		return
	}
	for _, v := range entries {
		if v.InternalEvent() {
			continue
		}
		entryView, err := c.history.ReadView(c.ctx, v)
		if err != nil {
			c.add("History presentation failed: "+err.Error(), 0)
			return
		}
		if entryView.StatusType == "system_prompt" && !c.showPromptMetadata(v.ID, entryView.Prompt) {
			continue
		}
		item := c.attribute(line{text: entryView.Label, id: v.ID, system: v.Kind == "status" || v.Role == "system"}, v.Actor)
		if v.Kind == "tool_result" || entryView.StatusType == "job_completion" {
			item.markdown, item.brief, item.system = true, true, false
		}
		if (v.Kind == "message" || v.Kind == "summary") && entryView.Message != nil {
			message := *entryView.Message
			if v.Kind == "message" && message.Type == llm.TypeUserInput {
				// Original inputs take their commit time from the entry, not
				// caller-supplied message metadata.
				message.InputTimeMS = v.CreatedMS
			}
			item = messageLine(item, message)
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
					item = c.attribute(line{text: "**image_show** · " + render.Inline(card.Snapshot.Path), id: card.EntryID, callID: card.Snapshot.ID, markdown: true, brief: true, image: &card.Snapshot}, card.Snapshot.Actor)
					c.view.replace(i, item)
					present = true
					break
				}
			}
			if !present {
				c.view.append(c.attribute(line{text: "**image_show** · " + render.Inline(card.Snapshot.Path), id: card.EntryID, callID: card.Snapshot.ID, markdown: true, brief: true, image: &card.Snapshot}, card.Snapshot.Actor))
			}
		}
	}
	c.focused = len(c.view.lines) - 1
	c.view.followTail = true
}
