package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v2"
	"scicode/internal/provider"
)

type namedPendingProvider struct {
	provider.Script
	release chan struct{}
	started bool
}

func (p *namedPendingProvider) Stream(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
	if strings.Contains(req.System, "\nYou are an isolated child agent.") && !p.started {
		p.started = true
		if err := emit(provider.StreamEvent{Kind: "call_start", CallStart: &provider.ToolStart{ID: "read", Name: "read"}}); err != nil {
			return err
		}
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.Script.Stream(ctx, req, emit)
}

func TestSubagentAwaitingToolIsNamedAndInspectable(t *testing.T) {
	p := &namedPendingProvider{release: make(chan struct{}), Script: provider.Script{Responses: []provider.ScriptResponse{
		{Calls: []provider.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"read missing file","label":"one two three four"}`)}}},
		{Calls: []provider.ToolCall{{ID: "read", Name: "read", Arguments: []byte(`{"path":"missing.txt"}`)}}},
		{Text: "Child handled missing file"}, {Text: "Parent finished"},
	}}}
	u := newQuestionTestUI(t, p)
	u.screen.SetSize(140, 45)
	u.screen.PostEventWait(tcell.NewEventResize(140, 45))
	u.typeText("start child")
	u.key(tcell.KeyEnter)
	u.wait(t, "[Sub one two three four] awaiting read ...")
	var id int64
	if err := u.runtime.Store.DB.QueryRow("SELECT id FROM entries WHERE json_extract(content_json,'$.type')='tool_stream'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	u.typeText(fmt.Sprintf("/inspect %d", id))
	u.key(tcell.KeyEnter)
	u.wait(t, "[Sub one two three four] Tool announced")
	close(p.release)
	u.wait(t, "[Sub one two three four] read")
	u.key(tcell.KeyEscape)
	u.wait(t, "Child handled missing file")
}

func TestSubagentBadgeRowsPreserveGeometryAndIdentity(t *testing.T) {
	for _, width := range []int{1, 2, 12, 40, 80} {
		for _, item := range []line{
			{text: "Runtime context · inspect", system: true},
			{text: "**read** · `file.go`", markdown: true, brief: true},
			{text: "one\ntwo", speaker: "assistant", markdown: true},
			{text: "", speaker: "assistant", markdown: true},
		} {
			item.actor, item.subagentName, item.id = "main/child", "Research helper", 9
			v := transcriptOf([]line{item})
			rows := v.viewport(width, 20)
			count := 0
			for _, row := range rows {
				if row.id != 9 || ansi.StringWidth(row.text) > width {
					t.Fatal("lost identity or exceeded viewport", width, row)
				}
				if strings.Contains(ansi.Strip(row.text), "[Sub Research helper]") {
					count++
				}
			}
			if width == 80 && count != 1 {
				t.Fatal("badge repeated/absent", rows)
			}
		}
	}
}

func TestSubagentDisplayLiveInspectAndReload(t *testing.T) {
	p := &provider.Script{Responses: []provider.ScriptResponse{
		{Calls: []provider.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(`{"persistent":true,"prompt":"write file","label":"  research  helper "}`)}}},
		{Calls: []provider.ToolCall{{ID: "write", Name: "write", Arguments: []byte(`{"path":"child.txt","content":"child result"}`)}}},
		{Text: "Named child answer"}, {Text: "Parent answer"},
	}}
	u := newQuestionTestUI(t, p)
	u.screen.SetSize(140, 45)
	u.screen.PostEventWait(tcell.NewEventResize(140, 45))
	u.typeText("start child")
	u.key(tcell.KeyEnter)
	frame := u.wait(t, "[Sub research helper] assistant")
	if strings.Contains(frame, "main/child_") || !strings.Contains(frame, "[Sub research helper] Runtime context") || !strings.Contains(frame, "[Sub research helper] write") {
		t.Fatal(frame)
	}
	names, err := u.runtime.Store.SubagentNames(context.Background(), u.runtime.Current())
	if err != nil || len(names) != 1 {
		t.Fatal(names, err)
	}
	u.wait(t, "Turn complete")
	var id int64
	if err := u.runtime.Store.DB.QueryRow("SELECT r.entry_id FROM tool_records r JOIN tool_calls c ON c.id=r.call_id WHERE c.name='write'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	u.typeText("/inspect " + strconv.FormatInt(id, 10))
	u.key(tcell.KeyEnter)
	u.wait(t, "[Sub research helper] write")
	u.key(tcell.KeyEscape)
	u.typeText("/load " + u.runtime.Current())
	u.key(tcell.KeyEnter)
	frame = u.wait(t, "[Sub research helper] assistant")
	if strings.Contains(frame, "main/child_") {
		t.Fatal(frame)
	}
}
