package tui

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"ttc/internal/capture"
	"ttc/internal/graphics"
	"ttc/internal/jobs"
	"ttc/internal/render"
	"ttc/internal/session"
)

func TestToolInspectionDrawKeepsFieldsCompact(t *testing.T) {
	detail := render.Tool("read", []byte(`{"path":"note.txt"}`), []byte(`{"content":"first\nsecond\n","offset":1,"truncated":false}`)).Detail
	for _, kitty := range []bool{false, true} {
		t.Run(fmt.Sprintf("kitty=%v", kitty), func(t *testing.T) {
			screen := newTestScreen(t)
			if err := screen.Init(); err != nil {
				t.Fatal(err)
			}
			defer screen.Fini()
			screen.SetSize(100, 40)
			var renderer *imageRenderer
			if kitty {
				var wire bytes.Buffer
				renderer = &imageRenderer{graphics: graphics.New(&wire, false), cellWidth: 8, cellHeight: 16}
			}
			window := &Window{Title: "Read", Text: detail, Markdown: true, renderer: renderer}
			if err := draw(screen, drawState{view: newTranscript(), sidebar: newSidebar(), focused: -1, draft: newComposer(""), renderer: renderer, window: window}); err != nil {
				t.Fatal(err)
			}
			frame := screenText(screen)
			rows := strings.Split(frame, "\n")
			for i, row := range rows {
				if strings.Contains(row, "Offset:") {
					if i+1 >= len(rows) || !strings.Contains(rows[i+1], "Truncated:") {
						t.Fatalf("read result fields are not adjacent:\n%s", frame)
					}
					return
				}
			}
			t.Fatalf("read result fields missing:\n%s", frame)
		})
	}
}

func TestShellInspectionDrawKeepsMetadataRowsAdjacent(t *testing.T) {
	manager := jobs.New(context.Background(), nil)
	defer manager.Close()
	id, err := manager.Start("main", "printf 'spacing-output'", t.TempDir(), 0, true, false, capture.OutputOptions{}, "spacing fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Wait(context.Background(), "main", id, nil); err != nil {
		t.Fatal(err)
	}
	runtime := &session.Runtime{Jobs: manager}
	detail := runtime.JobDetail(id, "")
	for _, kitty := range []bool{false, true} {
		for _, width := range []int{48, 100} {
			t.Run(fmt.Sprintf("kitty=%v/width=%d", kitty, width), func(t *testing.T) {
				screen := newTestScreen(t)
				if err := screen.Init(); err != nil {
					t.Fatal(err)
				}
				defer screen.Fini()
				screen.SetSize(width, 50)
				var renderer *imageRenderer
				if kitty {
					var wire bytes.Buffer
					renderer = &imageRenderer{graphics: graphics.New(&wire, false), cellWidth: 8, cellHeight: 16}
				}
				window := &Window{Title: "Shell execution", Text: detail, Markdown: true, renderer: renderer}
				if err := draw(screen, drawState{view: newTranscript(), sidebar: newSidebar(), focused: -1, draft: newComposer(""), renderer: renderer, window: window}); err != nil {
					t.Fatal(err)
				}
				frame := screenText(screen)
				rows := strings.Split(frame, "\n")
				start, end := -1, -1
				for i, row := range rows {
					if strings.Contains(row, "Job id:") {
						start = i
					}
					if strings.Contains(row, "Kind:") {
						end = i
					}
				}
				if start < 0 || end != start+1 {
					t.Fatalf("shell metadata rows are not adjacent (id=%d kind=%d):\n%s", start, end, frame)
				}
				for _, want := range []string{"Command:", "Running time:", "spacing-output"} {
					if !strings.Contains(frame, want) {
						t.Fatalf("shell inspector lost %q:\n%s", want, frame)
					}
				}
			})
		}
	}
}
