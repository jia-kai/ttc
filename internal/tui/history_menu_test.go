package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"scicode/internal/history"
	"scicode/internal/provider"
)

func TestHistoryMenuSerialInputsStayFlatAndKeepCheckpoints(t *testing.T) {
	m := newHistoryMenu(history.BranchTree{Current: 5, Nodes: []history.BranchNode{
		{ID: 0, Restorable: true},
		{ID: 1, Label: "First", UserInput: true, Restorable: true},
		{ID: 2, Parent: 1, Label: "Reply", Restorable: true},
		{ID: 3, Parent: 2, Label: "Second", UserInput: true, Restorable: true},
		{ID: 4, Parent: 3, Label: "Tool", Restorable: true},
		{ID: 5, Parent: 4, Label: "Third", UserInput: true, Restorable: true},
	}})
	if want := []string{"  #1 · First", "  #3 · Second", "> #5 · Third · current"}; !reflect.DeepEqual(m.rows, want) {
		t.Fatalf("serial input labels are not flat: %q", m.rows)
	}
	m.key(tcell.NewEventKey(tcell.KeyLeft, 0, 0), 10)
	if m.nodes[m.selected].ID != 3 {
		t.Fatal("flattening changed parent navigation")
	}
	if action, id := m.key(tcell.NewEventKey(tcell.KeyEnter, 0, 0), 10); action != "restore" || id != 2 {
		t.Fatal("flattening changed pre-input checkpoint", action, id)
	}
	m.key(tcell.NewEventKey(tcell.KeyRight, 0, 0), 10)
	if action, id := m.key(tcell.NewEventKey(tcell.KeyRune, ' ', 0), 10); action != "inspect" || id != 5 {
		t.Fatal("flattening changed inspection", action, id)
	}
}

func TestHistoryMenuNestedForksKeepSerialLanes(t *testing.T) {
	tree := history.BranchTree{Current: 8, Nodes: []history.BranchNode{{ID: 0, Restorable: true}}}
	for id, parent := range []int64{0, 1, 2, 3, 4, 3, 1, 7} {
		tree.Nodes = append(tree.Nodes, history.BranchNode{ID: int64(id + 1), Parent: parent, Label: "Input", UserInput: true, Restorable: true})
	}
	m := newHistoryMenu(tree)
	want := []string{
		"  #1 · Input",
		"  ├─ #2 · Input",
		"  │  #3 · Input",
		"  │  ├─ #4 · Input",
		"  │  │  #5 · Input",
		"  │  └─ #6 · Input",
		"  └─ #7 · Input",
		">    #8 · Input · current",
	}
	if !reflect.DeepEqual(m.rows, want) {
		t.Fatalf("fork connectors or serial lanes changed:\ngot %q\nwant %q", m.rows, want)
	}
	// Mouse selection follows wrapped rows, including the continuation columns.
	const width, height = 24, 22
	left, top, outerWidth, outerHeight := windowBounds(width, height)
	inner := outerWidth - 2
	headers := len(m.Window.HeaderLines(inner, outerHeight-3))
	row := 0
	for _, text := range m.rows[:4] {
		row += len(wrap(text, inner))
	}
	m.mouse(tcell.NewEventMouse(left+1, top+headers+row+1, tcell.Button1, 0), width, height)
	if m.nodes[m.selected].ID != 5 {
		t.Fatal("fork wrapping broke mouse selection", m.nodes[m.selected].ID)
	}
}

func TestHistoryMenuBaselineForkAndDeepForks(t *testing.T) {
	m := newHistoryMenu(history.BranchTree{Nodes: []history.BranchNode{
		{ID: 0, Restorable: true},
		{ID: 1, Label: "First branch", UserInput: true, Restorable: true},
		{ID: 2, Parent: 1, Label: "Serial", UserInput: true, Restorable: true},
		{ID: 3, Label: "Other branch", UserInput: true, Restorable: true},
	}})
	if !strings.Contains(m.rows[0], "├─ #1") || !strings.Contains(m.rows[1], "│  #2") || !strings.Contains(m.rows[2], "└─ #3") {
		t.Fatal("baseline alternatives were flattened", m.rows)
	}
	tree := history.BranchTree{Nodes: []history.BranchNode{{ID: 0, Restorable: true}}}
	parent := int64(0)
	for depth := 0; depth < 100; depth++ {
		first := int64(len(tree.Nodes))
		for _, id := range []int64{first, first + 1} {
			tree.Nodes = append(tree.Nodes, history.BranchNode{ID: id, Parent: parent, Label: "Input", UserInput: true, Restorable: true})
		}
		parent = first
	}
	tree.Current = parent
	m = newHistoryMenu(tree)
	for i, node := range m.nodes {
		// Box drawing may be wide under an East Asian terminal setting. Bound
		// the prefix independently of that process-wide display preference.
		if utf8.RuneCountInString(m.indents[i].prefix()) > 29 {
			t.Fatalf("unbounded fork indentation at #%d", node.ID)
		}
	}
	if !strings.Contains(m.Window.Text, "… ├─ #") {
		t.Fatal("deep forks do not disclose clipped lanes")
	}
}

func TestHistoryMenuBranchNavigationAndInspectOnly(t *testing.T) {
	tree := history.BranchTree{Name: "Research", Current: 3, Nodes: []history.BranchNode{
		{ID: 0, Label: "Start", Reason: "read-only baseline"},
		{ID: 1, Label: "Question", Restorable: true, UserInput: true},
		{ID: 2, Parent: 1, Label: "First branch", Restorable: true, UserInput: true},
		{ID: 3, Parent: 1, Label: "Second branch", Restorable: true, UserInput: true},
	}}
	m := newHistoryMenu(tree)
	key := func(k tcell.Key) (string, int64) { return m.key(tcell.NewEventKey(k, 0, 0), 8) }
	key(tcell.KeyLeft)
	if m.nodes[m.selected].ID != 1 {
		t.Fatal("left did not choose parent")
	}
	key(tcell.KeyRight)
	if action, id := key(tcell.KeyEnter); action != "restore" || id != 1 {
		t.Fatal(action, id)
	}
	key(tcell.KeyDown)
	if action, id := m.key(tcell.NewEventKey(tcell.KeyRune, ' ', 0), 8); action != "inspect" || id != 3 {
		t.Fatal(action, id)
	}
	key(tcell.KeyHome)
	if action, id := key(tcell.KeyEnter); action != "inspect" || id != 1 {
		t.Fatal(action, id)
	}
}

func BenchmarkHistoryMenuSerialNavigation(b *testing.B) {
	tree := history.BranchTree{Current: 100000, Nodes: []history.BranchNode{{ID: 0, Restorable: true}}}
	for id := int64(1); id <= tree.Current; id++ {
		tree.Nodes = append(tree.Nodes, history.BranchNode{ID: id, Parent: id - 1, UserInput: true, Restorable: true, Label: fmt.Sprintf("Input %d", id)})
	}
	m := newHistoryMenu(tree)
	ev := tcell.NewEventKey(tcell.KeyUp, 0, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.key(ev, 30)
	}
}

func TestHistoryMenuLongHistoryHasBoundedWindow(t *testing.T) {
	tree := history.BranchTree{Name: "Long history", Current: 100000}
	tree.Nodes = append(tree.Nodes, history.BranchNode{Label: "Start"})
	for id := int64(1); id <= tree.Current; id++ {
		tree.Nodes = append(tree.Nodes, history.BranchNode{ID: id, Parent: id - 1, Restorable: true, UserInput: true, Label: strings.Repeat("message ", 24)})
	}
	m := newHistoryMenu(tree)
	for i, node := range m.nodes {
		if m.indents[i].depth != 0 {
			t.Fatalf("serial history acquired branch depth at #%d", node.ID)
		}
	}
	for i := 0; i < 100; i++ {
		m.key(tcell.NewEventKey(tcell.KeyPgUp, 0, 0), 30)
		m.reveal(40, 15)
		if len(m.rows) > historyWindowRows || len(m.Window.Text) > historyWindowRows*300 {
			t.Fatal("rewrapped unbounded history", len(m.rows), len(m.Window.Text))
		}
	}
	if m.Window.Scroll < 0 || !strings.Contains(m.Window.Header, "of 100000") {
		t.Fatal(m.Window.Header, m.Window.Scroll)
	}
}

func TestHistoryPickerInspectionPreservesDraft(t *testing.T) {
	u := newQuestionTestUI(t, &provider.Script{Responses: []provider.ScriptResponse{{Text: "Original branch reply"}}})
	u.typeText("Start research")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	u.typeText("Retained draft")
	u.key(tcell.KeyCtrlX)
	u.typeText("g")
	u.wait(t, "Enter undo to input")
	u.key(tcell.KeyHome)
	u.typeText(" ")
	u.wait(t, "user · Start research · Esc closes")
	u.key(tcell.KeyEscape)
	u.wait(t, "Enter undo to input")
	u.key(tcell.KeyEscape)
	u.wait(t, "> Retained draft")
	var count int
	if err := u.runtime.Store.DB.QueryRow("SELECT count(*) FROM turns").Scan(&count); err != nil || count != 1 {
		t.Fatal("inspection submitted the draft", count, err)
	}
}

func TestHistoryMenuProjectsHumanBranchesAndCheckpointRestrictions(t *testing.T) {
	tree := history.BranchTree{Name: "Projected", Current: 6, Nodes: []history.BranchNode{
		{ID: 0, Label: "Baseline"},
		{ID: 1, Parent: 0, Label: "System prompt", Reason: "before undo boundary"},
		{ID: 2, Parent: 1, Label: "First input", UserInput: true, Restorable: true},
		{ID: 3, Parent: 2, Label: "Assistant", Restorable: true},
		{ID: 4, Parent: 3, Label: "Runtime notice", Role: "user", Restorable: true},
		{ID: 5, Parent: 4, Label: "Second input", UserInput: true, Restorable: true},
		{ID: 6, Parent: 5, Label: "Tool result", Restorable: true},
		{ID: 7, Parent: 2, Label: "Sibling input", UserInput: true, Restorable: true},
	}}
	m := newHistoryMenu(tree)
	if len(m.nodes) != 3 || m.nodes[m.selected].ID != 5 || m.nodes[1].Parent != 2 || m.nodes[2].Parent != 2 {
		t.Fatal("hidden events leaked into input tree", m.nodes)
	}
	if action, id := m.key(tcell.NewEventKey(tcell.KeyEnter, 0, 0), 10); action != "restore" || id != 4 {
		t.Fatal("did not restore pre-input checkpoint", action, id)
	}
	m.key(tcell.NewEventKey(tcell.KeyLeft, 0, 0), 10)
	if m.nodes[m.selected].Restorable || m.nodes[m.selected].Reason != "before undo boundary" {
		t.Fatal("input's checkpoint restriction was lost")
	}
	m.key(tcell.NewEventKey(tcell.KeyEnd, 0, 0), 10)
	if action, id := m.key(tcell.NewEventKey(tcell.KeyEnter, 0, 0), 10); action != "restore" || id != 2 {
		t.Fatal("projected ancestor restriction incorrectly inherited", action, id)
	}
	empty := newHistoryMenu(history.BranchTree{Nodes: tree.Nodes[:2]})
	if len(empty.nodes) != 0 || empty.Window.Text != "No user inputs" {
		t.Fatal("empty tree exposes non-user baseline", empty.Window.Text)
	}
}

func TestHistoryPickerRestoresUserCheckpointAndRedoFiles(t *testing.T) {
	p := &provider.Script{Responses: []provider.ScriptResponse{
		{Calls: []provider.ToolCall{{ID: "first", Name: "write", Arguments: []byte(`{"path":"note.txt","content":"first"}`)}}},
		{Text: "First input result."},
		{Calls: []provider.ToolCall{{ID: "second", Name: "write", Arguments: []byte(`{"path":"note.txt","content":"second"}`)}}},
		{Text: "Second input result."},
	}}
	u := newQuestionTestUI(t, p)
	for i, text := range []string{"Write first note.", "Write second note."} {
		u.typeText(text)
		u.key(tcell.KeyEnter)
		u.wait(t, []string{"First input result.", "Second input result."}[i])
		// An earlier turn's completion remains in the transcript. Wait for
		// this turn's completion frame before opening an idle-only picker.
		deadline := time.After(3 * time.Second)
	completion:
		for {
			select {
			case frame := <-u.screen.frames:
				if strings.Count(frame, "Turn complete") == i+1 {
					break completion
				}
			case <-deadline:
				t.Fatal("current turn did not reach its completion frame")
			}
		}
	}
	path := filepath.Join(u.runtime.Workspace.Root, "note.txt")
	if data, err := os.ReadFile(path); err != nil || string(data) != "second" {
		t.Fatal("second turn did not write", string(data), err)
	}
	u.key(tcell.KeyCtrlX)
	u.typeText("g")
	frame := u.wait(t, "Enter undo to input")
	// Transcript rows can remain visible outside the popup. Picker rows have
	// an entry ID followed by " · "; exclude non-human entries there.
	if !strings.Contains(frame, "Write first note.") || !strings.Contains(frame, "Write second note.") || strings.Contains(frame, "· System prompt") || strings.Contains(frame, "· tool_result") {
		t.Fatal("picker did not show just human inputs", frame)
	}
	waitRestore := func(generation uint64, name string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case frame = <-u.screen.frames:
				if u.runtime.Generation() > generation && strings.Contains(frame, name) && strings.Contains(frame, "First input result.") && !strings.Contains(frame, "Second input result.") && !strings.Contains(frame, "Enter undo to input") {
					return
				}
			case <-deadline:
				t.Fatal("checkpoint restore did not replay the first turn", frame)
			}
		}
	}
	// Unique metadata appears only when the command result replays history.
	// A generation change alone can race a queued frame from the previous view.
	setReplayName := func(name string) {
		t.Helper()
		if _, err := u.runtime.Store.DB.Exec("UPDATE sessions SET name=? WHERE id=?", name, u.runtime.Current()); err != nil {
			t.Fatal(err)
		}
	}
	setReplayName("First checkpoint restore")
	generation := u.runtime.Generation()
	u.key(tcell.KeyEnter) // selected second input; restore before its edits
	waitRestore(generation, "First checkpoint restore")
	if data, err := os.ReadFile(path); err != nil || string(data) != "first" {
		t.Fatal("checkpoint did not undo second turn", string(data), err)
	}
	// Re-selecting the same checkpoint must preserve the recorded redo suffix.
	u.key(tcell.KeyCtrlX)
	u.typeText("g")
	u.wait(t, "Enter undo to input")
	u.key(tcell.KeyEnd)
	setReplayName("Repeated checkpoint restore")
	generation = u.runtime.Generation()
	u.key(tcell.KeyEnter)
	waitRestore(generation, "Repeated checkpoint restore")
	u.typeText("/redo")
	u.key(tcell.KeyEnter)
	u.wait(t, "redo completed")
	if data, err := os.ReadFile(path); err != nil || string(data) != "second" {
		t.Fatal("redo did not restore suffix", string(data), err)
	}
}
