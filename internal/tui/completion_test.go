package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v3"
	"golang.org/x/sys/unix"
	"ttc/internal/llm"
)

func TestPathCompletionRejectsFIFOAndWorkerStillCloses(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	q := completionQuery{root: root, prefix: "fifo/", marker: '@'}
	result := make(chan completionResult, 1)
	go func() { result <- listPaths(context.Background(), q) }()
	select {
	case got := <-result:
		if got.err == nil || !strings.Contains(got.err.Error(), "requires a directory") {
			t.Fatalf("FIFO directory completion must fail explicitly: %+v", got)
		}
	case <-time.After(time.Second):
		// Release an old blocking-open implementation before failing, so the
		// regression does not leak its worker or hang test cleanup.
		f, err := os.OpenFile(path, os.O_WRONLY|unix.O_NONBLOCK, 0600)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		<-result
		t.Fatal("path completion blocked opening a FIFO")
	}
	p := newPathCompleter(context.Background())
	p.query(q)
	select {
	case got := <-p.results:
		if got.err == nil {
			t.Fatal("completion worker accepted a FIFO directory")
		}
	case <-time.After(time.Second):
		t.Fatal("completion worker produced no FIFO error")
	}
	p.close()
}

func TestCompletionTokensAndCursorReplacement(t *testing.T) {
	for _, s := range []string{"email@host", "text /help", "literal @\"file name\""} {
		if _, ok := completionAt(newComposer(s), t.TempDir(), 1); ok {
			t.Fatal("unexpected completion", s)
		}
	}
	c := newComposer("/he rest")
	c.cursor = 3
	q, ok := completionAt(c, "", 1)
	if !ok {
		t.Fatal("no slash completion")
	}
	menu := completionMenu{result: commandCompletions(q)}
	if _, ok := menu.accept(&c); !ok || c.text != "/help rest" || c.cursor != 5 {
		t.Fatal(c)
	}
	c = newComposer("see @fi trailing")
	c.cursor = 7
	q, ok = completionAt(c, "", 1)
	if !ok {
		t.Fatal("no file completion")
	}
	menu.result = completionResult{query: q, items: []completionItem{{value: "file name.txt"}}}
	path, ok := menu.accept(&c)
	if !ok || path != "file name.txt" || !strings.Contains(c.text, "@\"file name.txt\"") {
		t.Fatal(path, c)
	}
}

func TestModelCompletionReplacesLegacyEasyCommand(t *testing.T) {
	model := commandCompletions(completionQuery{prefix: "model"})
	if len(model.items) == 0 || model.items[0].value != "/model" || !strings.Contains(model.items[0].description, "easy") {
		t.Fatalf("model completion = %+v", model.items)
	}
	if legacy := commandCompletions(completionQuery{prefix: "easy-model"}); len(legacy.items) != 0 {
		t.Fatalf("legacy easy command completion = %+v", legacy.items)
	}
}

func TestForceCompactCompletionAndCommandMenuShareDescription(t *testing.T) {
	draft := newComposer("/force-c")
	q, ok := completionAt(draft, "", 1)
	if !ok {
		t.Fatal("force-compaction prefix did not activate command completion")
	}
	result := commandCompletions(q)
	if len(result.items) != 1 || result.items[0].value != "/force-compact" {
		t.Fatalf("force-compaction completion = %+v", result.items)
	}
	item := result.items[0]
	if !strings.Contains(item.description, "input estimate") || !strings.Contains(item.description, "provider may reject") {
		t.Fatal("completion implies guaranteed or unbounded compaction", item.description)
	}
	completion := completionMenu{result: result}
	if _, ok := completion.accept(&draft); !ok || strings.TrimSpace(draft.text) != "/force-compact" {
		t.Fatal("force-compaction completion was not accepted", draft.text)
	}
	menu := newCommandMenu()
	if !strings.Contains(menu.Window.Text, item.value+" · "+item.description) {
		t.Fatal("command menu omitted force-compaction or changed its description")
	}
	for i, command := range slashCommands {
		if command.value == item.value {
			menu.selected = i
			break
		}
	}
	command, closed := menu.key(tcell.NewEventKey(tcell.KeyEnter, "", 0), 10)
	if command != "/force-compact" || !closed {
		t.Fatal("menu did not select force-compaction", command, closed)
	}
}

func TestCompletionModifiedNavigationAndNewline(t *testing.T) {
	for _, text := range []string{"/he", "draft @fi"} {
		t.Run(text, func(t *testing.T) {
			draft := newComposer(text)
			q, ok := completionAt(draft, t.TempDir(), 1)
			if !ok {
				t.Fatal("missing completion query")
			}
			menu := &completionMenu{result: completionResult{query: q, items: []completionItem{{value: "first"}, {value: "second"}}}}
			c := frontendController{draft: draft, completion: menu}
			c.draft.completion = menu
			for _, key := range []tcell.Key{tcell.KeyUp, tcell.KeyDown} {
				if c.handleCompletionKey(tcell.NewEventKey(key, "", 0)) || menu.selected != 0 || c.draft.text != text {
					t.Fatal("plain arrow intercepted by suggestions", key, menu.selected, c.draft.text)
				}
			}
			if !c.handleCompletionKey(tcell.NewEventKey(tcell.KeyDown, "", tcell.ModShift)) || menu.selected != 1 {
				t.Fatal("Shift-Down did not select next suggestion", menu.selected)
			}
			if !c.handleCompletionKey(tcell.NewEventKey(tcell.KeyUp, "", tcell.ModShift)) || menu.selected != 0 {
				t.Fatal("Shift-Up did not select previous suggestion", menu.selected)
			}
			newline := tcell.NewEventKey(tcell.KeyEnter, "", tcell.ModShift)
			if c.handleCompletionKey(newline) || !c.draft.key(newline) || c.draft.text != text+"\n" {
				t.Fatal("Shift-Enter accepted a suggestion instead of inserting newline", c.draft.text)
			}
		})
	}
}

func TestCompletionDoesNotInterceptHistoryOrShiftEnter(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{{Text: "Remembered."}}})
	u.typeText("remember")
	u.key(tcell.KeyEnter)
	u.wait(t, "Turn complete")
	u.typeText("/he")
	u.wait(t, "Keyboard and command guide")
	u.key(tcell.KeyUp)
	u.wait(t, "> remember")
	u.key(tcell.KeyDown)
	u.wait(t, "> /he")
	u.screen.EventQ() <- tcell.NewEventKey(tcell.KeyEnter, "", tcell.ModShift)
	u.typeText("tail")
	waitComposerRows(t, u, "> /he", "  tail")
	assertComposerSubmission(t, u, "remember")
}

func TestPathCompletionBoundAndWorkerShutdown(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"alpha.txt", "beta.txt", "a dir"} {
		if strings.Contains(name, "dir") {
			os.Mkdir(filepath.Join(root, name), 0700)
		} else {
			os.WriteFile(filepath.Join(root, name), []byte(name), 0600)
		}
	}
	q := completionQuery{root: root, prefix: "a", marker: '@'}
	result := listPaths(context.Background(), q)
	if len(result.items) != 3 || !result.items[0].directory || result.items[1].value != "beta.txt" || result.items[2].value != "alpha.txt" {
		t.Fatal(result)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if listPaths(ctx, q).err == nil {
		t.Fatal("ignored cancellation")
	}
	p := newPathCompleter(context.Background())
	for i := range 20 {
		q.cursor = i
		p.query(q)
	}
	select {
	case result := <-p.results:
		if result.query.cursor != 19 {
			t.Fatal("not latest", result)
		}
	case <-time.After(time.Second):
		t.Fatal("no path results")
	}
	p.close()
}

func TestSlashDropdownAndAttachmentSnapshot(t *testing.T) {
	u := newQuestionTestUI(t, &llm.Script{Responses: []llm.ScriptResponse{{Text: "Attached result"}}})
	if err := os.WriteFile(filepath.Join(u.runtime.Workspace.Root, "fixture.txt"), []byte("snapshot evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	u.typeText("/he")
	u.wait(t, "Keyboard and command guide")
	u.key(tcell.KeyTab)
	u.key(tcell.KeyEnter)
	u.wait(t, "TTC help")
	u.key(tcell.KeyEscape)
	u.typeText("look @IXT")
	u.wait(t, "fixture.txt")
	u.key(tcell.KeyTab)
	u.wait(t, "Attached ·")
	os.WriteFile(filepath.Join(u.runtime.Workspace.Root, "fixture.txt"), []byte("changed later"), 0600)
	u.key(tcell.KeyEnter)
	u.wait(t, "Attached result")
	entries, err := u.runtime.Store.Branch(u.runtime.Current(), 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.Role == "user" && strings.Contains(string(entry.Content), "snapshot evidence") {
			found = true
			if strings.Contains(string(entry.Content), "changed later") {
				t.Fatal("attachment not frozen")
			}
		}
	}
	if !found {
		t.Fatal("missing attachment")
	}
}

func TestPathSearchSubstringsTermsAndRelativeOrdering(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"zz.txt", "aa.txt", "long-report.txt", "nested/Report-DRAFT.txt", "nested/other.txt"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(root, filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		query string
		want  []string
	}{
		{"TXT", []string{"aa.txt", "zz.txt", "long-report.txt", "nested/other.txt", "nested/Report-DRAFT.txt"}},
		{"port", []string{"long-report.txt", "nested/Report-DRAFT.txt"}},
		{"DRAFT port", []string{"nested/Report-DRAFT.txt"}},
		{"nested/RAFT", []string{"nested/Report-DRAFT.txt"}},
		{"other nested", []string{"nested/other.txt"}},
		{"missing", nil},
	} {
		t.Run(tt.query, func(t *testing.T) {
			result := listPaths(context.Background(), completionQuery{root: root, prefix: tt.query, marker: '@'})
			if result.err != nil || result.truncated {
				t.Fatal(result)
			}
			var got []string
			for _, item := range result.items {
				got = append(got, item.value)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
	c := newComposer("@\"DRAFT port")
	q, ok := completionAt(c, root, 1)
	if !ok || q.prefix != "DRAFT port" {
		t.Fatal(q, ok)
	}
	m := completionMenu{result: listPaths(context.Background(), q)}
	if path, ok := m.accept(&c); !ok || path != "nested/Report-DRAFT.txt" || c.text != "@nested/Report-DRAFT.txt " {
		t.Fatal(path, ok, c)
	}
}

func TestPathSearchLimitsAfterSorting(t *testing.T) {
	root := t.TempDir()
	for i := range 300 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("long-match-%03d.txt", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Created last, but must not be discarded by the match cap.
	if err := os.WriteFile(filepath.Join(root, "match"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	result := listPaths(context.Background(), completionQuery{root: root, prefix: "match", marker: '@'})
	if result.err != nil || !result.truncated || len(result.items) != 256 || result.items[0].value != "match" || result.items[255].value != "long-match-254.txt" {
		t.Fatal(result)
	}
}

func TestPathSearchSkipsUnreadableDescendants(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode-000 directories")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0700); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "report.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	result := listPaths(context.Background(), completionQuery{root: root, prefix: "port", marker: '@'})
	if result.err != nil || !result.truncated || len(result.items) != 1 || result.items[0].value != "report.txt" {
		t.Fatal(result)
	}
	result = listPaths(context.Background(), completionQuery{root: root, prefix: "locked/", marker: '@'})
	if result.err == nil {
		t.Fatal("explicit unreadable search root must fail")
	}
}

func TestQuotedDirectoryNavigationAndAttachment(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a dir")
	os.Mkdir(dir, 0700)
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("evidence"), 0600)
	c := newComposer("@a")
	q, _ := completionAt(c, root, 1)
	m := completionMenu{result: completionResult{query: q, items: []completionItem{{value: "a dir/", directory: true}}}}
	if path, ok := m.accept(&c); !ok || path != "" {
		t.Fatal(path, c)
	}
	q, ok := completionAt(c, root, 1)
	if !ok || q.prefix != "a dir/" {
		t.Fatal("lost quoted directory", c, q)
	}
	result := listPaths(context.Background(), q)
	index := -1
	for i, item := range result.items {
		if item.value == "a dir/file.txt" {
			index = i
		}
	}
	if index < 0 {
		t.Fatal(result)
	}
	m.result, m.selected = result, index
	path, ok := m.accept(&c)
	if !ok || path != "a dir/file.txt" || c.text != "@\"a dir/file.txt\" " {
		t.Fatal(path, c)
	}
}
