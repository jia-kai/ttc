package tui

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/scratch"
	"ttc/internal/session"
	"ttc/internal/skills"
	"ttc/internal/tool"
	"ttc/internal/workspace"
)

// helpTestProvider records inference requests and holds the first response until
// the test has inspected help and closed it without stopping the active turn.
type helpTestProvider struct {
	llm.Script
	requests chan llm.Request
	release  chan struct{}
	first    bool
}

func (p *helpTestProvider) Stream(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
	select {
	case p.requests <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	if !p.first {
		p.first = true
		select {
		case <-p.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p.Script.Stream(ctx, req, emit)
}

func helpTestRequest(t *testing.T, p *helpTestProvider, text string) {
	t.Helper()
	select {
	case req := <-p.requests:
		for i := len(req.Messages) - 1; i >= 0; i-- {
			m := req.Messages[i]
			if m.Role == "user" && !m.Runtime {
				if m.Content != text {
					t.Fatalf("latest input = %q, want %q", m.Content, text)
				}
				return
			}
		}
		t.Fatal("request has no human input")
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
}

func waitHelpClosed(t *testing.T, u *questionTestUI, needles ...string) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	var latest string
	for {
		select {
		case latest = <-u.screen.frames:
			if strings.Contains(latest, "Command result") {
				continue
			}
			matches := true
			for _, needle := range needles {
				matches = matches && strings.Contains(latest, needle)
			}
			if matches {
				return latest
			}
		case <-deadline:
			t.Fatalf("help did not close with %v: %s", needles, latest)
		}
	}
}

func TestBusyHelpPreservesTurnQueueSteersAndAttachments(t *testing.T) {
	p := &helpTestProvider{
		Script:   llm.Script{Responses: []llm.ScriptResponse{{Text: "Initial response."}, {Text: "Steer response."}, {Text: "Queue response."}}},
		requests: make(chan llm.Request, 8), release: make(chan struct{}),
	}
	u := newQuestionTestUI(t, p)
	u.typeText("initial")
	u.key(tcell.KeyEnter)
	helpTestRequest(t, p, "initial")
	u.typeText("steer instruction")
	u.key(tcell.KeyEnter)
	u.wait(t, "Steer · steer instruction")
	u.typeText("queued instruction")
	u.screen.PostEventWait(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModAlt))
	u.wait(t, "Queued · queued instruction")
	path := filepath.Join(u.runtime.Workspace.Root, "help-attachment.txt")
	if err := os.WriteFile(path, []byte("preserved snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	u.typeText("/attach " + path)
	u.key(tcell.KeyEnter)
	u.wait(t, "1 attachments")
	// This unpersisted status disappears if help unnecessarily replays history.
	u.runtime.Emit(session.Event{Kind: "status", Text: "STATUS_BEFORE_HELP", Generation: u.runtime.Generation()})
	u.wait(t, "STATUS_BEFORE_HELP")
	generation := u.runtime.Generation()
	u.typeText(" /help ")
	u.key(tcell.KeyEnter)
	u.wait(t, "Command result")
	u.wait(t, "TTC help")
	if count, _ := u.runtime.SteeringPreview(0); u.runtime.Generation() != generation || count != 1 {
		t.Fatal("help changed the generation or pending steer")
	}
	select {
	case req := <-p.requests:
		t.Fatal("help resumed inference", req)
	default:
	}
	u.key(tcell.KeyEscape)
	waitHelpClosed(t, u, "Working", "STATUS_BEFORE_HELP", "Queued · queued instruction", "Steer · steer instruction", "1 attachments")
	close(p.release)
	u.wait(t, "Queue response.")
	u.wait(t, "Turn complete")
	helpTestRequest(t, p, "steer instruction")
	helpTestRequest(t, p, "queued instruction")
	var requests, turns int
	if err := u.runtime.Store.DB.QueryRow("SELECT count(*), count(DISTINCT turn_id) FROM model_requests WHERE purpose='coding'").Scan(&requests, &turns); err != nil || requests != 3 || turns != 2 {
		t.Fatal("help changed inference or turn admission", requests, turns, err)
	}
	messages, err := u.runtime.Store.Messages(u.runtime.Current())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range messages {
		if m.Role == "user" && !m.Runtime && strings.TrimSpace(m.Content) == "/help" {
			t.Fatal("help entered model history")
		}
	}
}

func TestIdleHelpPreservesUnpersistedStatusWithoutInference(t *testing.T) {
	p := &helpTestProvider{requests: make(chan llm.Request, 8), release: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	u.runtime.Emit(session.Event{Kind: "status", Text: "IDLE_STATUS_BEFORE_HELP", Generation: u.runtime.Generation()})
	u.wait(t, "IDLE_STATUS_BEFORE_HELP")
	u.typeText("/help")
	u.key(tcell.KeyEnter)
	u.wait(t, "Command result")
	u.wait(t, "TTC help")
	u.key(tcell.KeyEscape)
	frame := waitHelpClosed(t, u, "IDLE_STATUS_BEFORE_HELP")
	if strings.Contains(frame, "Working") || strings.Contains(frame, "Turn complete") {
		t.Fatal("help created an active turn", frame)
	}
	select {
	case req := <-p.requests:
		t.Fatal("help started inference", req)
	default:
	}
	var sessions int
	if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM sessions").Scan(&sessions); err != nil || sessions != 0 {
		t.Fatal("help persisted the blank session", sessions, err)
	}
}

func TestPlainBusyHelpPrintsBeforeActiveResponseFinishes(t *testing.T) {
	if _, err := scratch.Verify(); err != nil {
		t.Fatal(err)
	}
	store, err := history.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	w, err := workspace.Open(t.TempDir(), store)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := skills.Discover(context.Background(), w.Root, "")
	if err != nil {
		t.Fatal(err)
	}
	p := &helpTestProvider{Script: llm.Script{Responses: []llm.ScriptResponse{{Text: "Plain response."}}}, requests: make(chan llm.Request, 8), release: make(chan struct{})}
	selection := llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan session.Event, 64)
	r, err := session.New(ctx, store, w, p, selection, "", catalog, tool.WebSearchConfig{}, func(e session.Event) {
		select {
		case events <- e:
		case <-ctx.Done():
		}
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	r.AutoName = false
	input, writer := io.Pipe()
	t.Cleanup(func() { input.Close(); writer.Close() })
	output := make(chan string, 128)
	f := Frontend{Runtime: r, Events: events, Input: input, Output: eventWriter{output}, Plain: true}
	stopped := make(chan struct{})
	go func() { defer close(stopped); _ = f.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Error("plain frontend did not stop")
		}
		r.Close()
	})
	if _, err := io.WriteString(writer, "initial\n"); err != nil {
		t.Fatal(err)
	}
	helpTestRequest(t, p, "initial")
	if _, err := io.WriteString(writer, "/help\n"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case text := <-output:
			if !strings.Contains(text, "# TTC help") {
				continue
			}
			if !strings.Contains(text, "## Send and stop") || strings.Contains(text, "Command result") {
				t.Fatal("plain help did not print Markdown directly", text)
			}
			select {
			case req := <-p.requests:
				t.Fatal("plain help resumed inference", req)
			default:
			}
			close(p.release)
			return
		case <-deadline:
			t.Fatal("plain help waited for the active response")
		}
	}
}
