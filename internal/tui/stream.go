package tui

import (
	"strings"

	"ttc/internal/llm"
	"ttc/internal/messagestream"
)

// streamLine is the single live/replay projection of a persisted boundary row.
// Its actions target immutable inspection records, not its correlation identity.
func streamLine(item messagestream.Item) line {
	row := line{id: item.ID, revision: item.Revision, actor: item.Actor, text: item.Title, system: true, brief: true}
	for _, action := range item.Actions {
		if action.Target <= 0 {
			continue
		}
		if action.Kind == messagestream.Raw {
			row.rawID = action.Target
			continue
		}
		if row.action != actionNone {
			continue
		}
		switch action.Kind {
		case messagestream.Inspect:
			row.action, row.inspectID = actionInspect, action.Target
			row.text += " · Inspect"
		default:
			continue
		}
	}
	return row
}

// publishStream updates the stable request row without appending separate
// response/outcome cards. Replayed revisions fence queued older publications.
func (c *frontendController) publishStream(item messagestream.Item) {
	if c.streamRows == nil {
		c.streamRows = map[int64]int{}
	}
	row := c.attribute(streamLine(item), item.Actor)
	if index, found := c.streamRows[item.ID]; found {
		if item.Revision <= c.view.lines[index].revision {
			return
		}
		c.view.replace(index, row)
		c.refreshRequestInspection(item.ID, row.inspectID)
		return
	}
	c.streamRows[item.ID] = len(c.view.lines)
	c.addActor(row, item.Actor)
}

// directMessageLine preserves readable runtime text without offering payload
// inspection, independently of whether boundary captures exist for the session.
func directMessageLine(item line, message llm.Message) line {
	item = messageLine(item, message)
	item.action, item.inspectID = actionNone, 0
	switch message.Type.Presentation() {
	case llm.PresentationInstruction, llm.PresentationContext:
		item.text = strings.TrimSuffix(item.text, " · inspect")
		if text := message.DisplayText(); text != "" {
			item.text += " · " + text
		}
	}
	return item
}
