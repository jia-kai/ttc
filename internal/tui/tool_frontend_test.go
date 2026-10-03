package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func TestJobReadCompletedRowRendersOutputThroughClickInspectAndLoad(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(fmt.Sprintf("paged=%v", large), func(t *testing.T) {
			p := &provider.Script{Responses: []provider.ScriptResponse{{Text: "Ready."}, {}, {Text: "Page captured."}}}
			u := newQuestionTestUI(t, p)
			u.typeText("Initialize capture history")
			u.key(tcell.KeyEnter)
			u.wait(t, "Turn complete")
			output := "OUTPUT-FIRST\n"
			if large {
				output += strings.Repeat("captured fixture\n", 1200)
			}
			output += "```\n$x^2$\nOUTPUT-LAST\n"
			job, err := u.runtime.Jobs.StartTask("main", "shell", "inspector fixture", false, false, func(_ context.Context, stdout, stderr io.Writer) error {
				if _, err := io.WriteString(stdout, output); err != nil {
					return err
				}
				_, err := io.WriteString(stderr, "UNREQUESTED-STDERR\n")
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := u.runtime.Jobs.Wait(context.Background(), "main", job, nil); err != nil {
				t.Fatal(err)
			}
			args, err := json.Marshal(map[string]any{"job_id": job, "limit_bytes": 22000})
			if err != nil {
				t.Fatal(err)
			}
			p.Responses[1] = provider.ScriptResponse{Calls: []provider.ToolCall{{ID: "page", Name: "job_read", Arguments: args}}}
			u.typeText("Read the captured output")
			u.key(tcell.KeyEnter)
			u.wait(t, "Page captured.")
			frame := u.wait(t, "Turn complete")
			var result int64
			if err := u.runtime.Store.DB.QueryRow("SELECT r.entry_id FROM tool_records r JOIN tool_calls c ON c.id=r.call_id WHERE c.name='job_read'").Scan(&result); err != nil {
				t.Fatal(err)
			}
			click := func(frame string) {
				t.Helper()
				for y, row := range strings.Split(frame, "\n") {
					if strings.Contains(row, "job_read") {
						u.screen.PostEventWait(tcell.NewEventMouse(3, y, tcell.Button1, 0))
						return
					}
				}
				t.Fatal("completed job_read row disappeared", frame)
			}
			check := func() {
				t.Helper()
				frame := u.wait(t, "OUTPUT-FIRST")
				if !strings.Contains(frame, "Output:") || strings.Contains(frame, "UNREQUESTED-STDERR") {
					t.Fatal("job_read inspector did not display its requested result", frame)
				}
				if large {
					u.typeText("]")
					u.wait(t, "page 2/2")
				}
				u.key(tcell.KeyEnd)
				frame = u.wait(t, "OUTPUT-LAST")
				if !strings.Contains(frame, "$x^2$") || strings.Contains(frame, "UNREQUESTED-STDERR") {
					t.Fatal("paging changed literal captured output", frame)
				}
				u.key(tcell.KeyEscape)
				u.wait(t, "Turn complete")
			}
			click(frame)
			check()
			u.typeText(fmt.Sprintf("/inspect %d", result))
			u.key(tcell.KeyEnter)
			check()
			// Reload closes the capture manager. Inspection must use the durable
			// output page, not a job handle or a fresh unrelated capture tail.
			u.typeText("/load " + u.runtime.Current())
			u.key(tcell.KeyEnter)
			frame = u.wait(t, "No request yet")
			click(frame)
			check()
		})
	}
}

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
