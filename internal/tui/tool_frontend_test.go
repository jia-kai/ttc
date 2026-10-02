package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v2"
	"scicode/internal/provider"
	"scicode/internal/render"
	"scicode/internal/tool"
)

func TestLiveToolCardInspectorRefreshesAndBecomesDurable(t *testing.T) {
	p := &provider.Script{Responses: []provider.ScriptResponse{{Calls: []provider.ToolCall{{ID: "stream", Name: "shell", Arguments: []byte(`{"command":"printf 'hello'"}`)}}}, {Text: "finished"}}}
	u := newQuestionTestUI(t, p)
	advance := make(chan struct{}, 2)
	u.runtime.Tools = tool.NewRegistry()
	tool.Register(u.runtime.Tools, "shell", "controlled streaming tool", map[string]any{"command": tool.Property("string")}, []string{"command"}, func(a struct {
		Command string `json:"command"`
	}) error {
		return nil
	}, func(ctx context.Context, x tool.Execution, a struct {
		Command string `json:"command"`
	}) (any, error) {
		for _, text := range []string{"first preview", "second preview"} {
			x.Update(map[string]any{"status": "running", "stdout": text})
			select {
			case <-advance:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return map[string]any{"status": "completed", "stdout": "final preview"}, nil
	})
	u.typeText("start")
	u.key(tcell.KeyEnter)
	u.wait(t, "first preview")
	var intent int64
	if err := u.runtime.Store.DB.QueryRow("SELECT id FROM entries WHERE kind='tool_call' ORDER BY id DESC LIMIT 1").Scan(&intent); err != nil {
		t.Fatal(err)
	}
	u.typeText(fmt.Sprintf("/inspect %d", intent))
	u.key(tcell.KeyEnter)
	u.wait(t, "Command:")
	advance <- struct{}{}
	u.wait(t, "second preview")
	advance <- struct{}{}
	u.wait(t, "final preview")
	// Synchronize with the persisted result, not a transient final status.
	deadline := time.Now().Add(3 * time.Second)
	var count int
	for time.Now().Before(deadline) {
		if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM tool_records").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if count != 1 {
		t.Fatal("updates created no final record", count)
	}
	u.key(tcell.KeyEscape)
	u.wait(t, "Turn complete")
	var final int64
	if err := u.runtime.Store.DB.QueryRow("SELECT entry_id FROM tool_records").Scan(&final); err != nil {
		t.Fatal(err)
	}
	u.typeText(fmt.Sprintf("/inspect %d", final))
	u.key(tcell.KeyEnter)
	u.wait(t, "final preview")
}

func TestRealShellInspectorUpdatesExpandedCaptureAndInterrupts(t *testing.T) {
	p := &provider.Script{Responses: []provider.ScriptResponse{{Calls: []provider.ToolCall{{ID: "real-stream", Name: "shell", Arguments: []byte(`{"command":"i=1; while [ $i -le 20 ]; do printf 'line-%02d\\n' \"$i\"; i=$((i+1)); done; while [ ! -f more ]; do sleep 0.01; done; cat more; sleep 30"}`)}}}}}
	u := newQuestionTestUI(t, p)
	u.typeText("real shell")
	u.key(tcell.KeyEnter)
	u.wait(t, "running")
	var intent int64
	if err := u.runtime.Store.DB.QueryRow("SELECT id FROM entries WHERE kind='tool_call' ORDER BY id DESC LIMIT 1").Scan(&intent); err != nil {
		t.Fatal(err)
	}
	u.typeText(fmt.Sprintf("/inspect %d", intent))
	u.key(tcell.KeyEnter)
	u.wait(t, "Command:")
	u.key(tcell.KeyEnd)
	u.wait(t, "line-20")
	u.wait(t, "line-01") // Expanded capture includes output absent from the card tail.
	if err := os.WriteFile(filepath.Join(u.runtime.Workspace.Root, "more"), []byte("new-output-from-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	u.wait(t, "new-output-from-file")
	u.key(tcell.KeyEscape)
	u.key(tcell.KeyEscape)
	u.key(tcell.KeyEscape)
	u.wait(t, "Turn interrupted")
	jobs := u.runtime.Jobs.List("main", true)
	if len(jobs) != 1 || jobs[0].Status != "cancelled" {
		t.Fatal(jobs)
	}
	var final int64
	if err := u.runtime.Store.DB.QueryRow("SELECT entry_id FROM tool_records").Scan(&final); err != nil {
		t.Fatal(err)
	}
	u.typeText(fmt.Sprintf("/inspect %d", final))
	u.key(tcell.KeyEnter)
	u.key(tcell.KeyEnd)
	u.wait(t, "new-output-from-file")
}

func TestToolBriefingsUseOneRowForUpdatesAndSavedCards(t *testing.T) {
	for _, status := range []string{"running", "completed"} {
		md := render.Tool("shell", []byte(`{"command":"printf '界é'"}`), []byte(fmt.Sprintf(`{"status":%q,"stdout":"one\ntwo\n","stderr":"error\n"}`, status)))
		for _, width := range []int{1, 8, 20, 100} {
			lines := []line{{text: md.Summary, id: 1, markdown: true, brief: true}, {text: "**glob** · ` *.go `", id: 2, markdown: true, brief: true}}
			got := transcriptOf(lines).viewport(width, 30)
			if len(got) != 2 || got[0].id != 1 || got[1].id != 2 {
				t.Fatal("cards expanded or lost identity", got)
			}
			for _, row := range got {
				if strings.ContainsRune(row.text, '\n') || ansi.StringWidth(row.text) > width {
					t.Fatal(width, row.text)
				}
			}
		}
	}
}
