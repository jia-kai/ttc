package context

import (
	stdcontext "context"
	"encoding/json"
	"os"
	"path/filepath"
	"scicode/internal/provider"
	"strings"
	"testing"
)

func TestWholeTurnRetentionAndBudgets(t *testing.T) {
	m := []provider.Message{{Role: "user", Content: strings.Repeat("a", 600)}, {Role: "assistant", Content: "old"}, {Role: "user", Content: "new"}, {Role: "assistant", Calls: []provider.ToolCall{{ID: "c", Name: "read", Arguments: []byte(`{"path":"x"}`)}}}, {Role: "tool", CallID: "c", Content: `{"ok":true}`}}
	cut, e := Retain(m, 200)
	if e != nil || cut.Start != 2 || cut.User != -1 {
		t.Fatalf("cut=%+v err=%v", cut, e)
	}
	sel := provider.Selection{Model: provider.ScriptModel()}
	if !Fits(sel, "instructions", nil, m, false) {
		t.Fatal("small request did not fit")
	}
	if Fits(sel, strings.Repeat("a", 100000), nil, m, false) {
		t.Fatal("oversize request fit")
	}
}

func TestRetentionKeepsLastTwoModelMessagesAndBalancedParallelTools(t *testing.T) {
	messages := []provider.Message{
		{Role: "user", Content: "Keep researching."},
		{Role: "assistant", Content: strings.Repeat("old ", 1000), Calls: []provider.ToolCall{{ID: "a"}, {ID: "b"}}},
		{Role: "tool", CallID: "b", Content: "second result"},
		{Role: "tool", CallID: "a", Content: "first result"},
		{Role: "assistant", Content: "Recent reasoning", Calls: []provider.ToolCall{{ID: "c"}}},
		{Role: "tool", CallID: "c", Content: "recent result"},
		{Role: "user", Runtime: true, Content: "A background job completed."},
		{Role: "assistant", Content: "Latest reasoning", Calls: []provider.ToolCall{{ID: "d"}}},
		{Role: "tool", CallID: "d", Content: "latest result"},
	}
	for _, target := range []int{200, 1} {
		retention, err := Retain(messages, target)
		if err != nil || retention.Start != 4 || retention.User != 0 {
			t.Fatalf("target %d: %+v, %v", target, retention, err)
		}
	}
	if _, err := Retain(messages[:len(messages)-1], 200); err == nil {
		t.Fatal("unresolved call was discarded")
	}
	if _, err := Retain([]provider.Message{{Role: "user", Content: "Only a prompt."}}, 1); err == nil {
		t.Fatal("compaction without model work reported progress")
	}
}
func TestAttachmentsSnapshotAndDirectoryNoSymlinkTraversal(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "x.txt")
	os.WriteFile(p, []byte("before"), 0600)
	a, e := Snapshot(stdcontext.Background(), p, false)
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(p, []byte("after"), 0600)
	if a.Text != "before" {
		t.Fatal("mutable snapshot")
	}
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0600)
	os.Symlink(outside, filepath.Join(root, "link"))
	dir, e := Snapshot(stdcontext.Background(), root, false)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(dir.Text, "secret") {
		t.Fatal("followed symlink")
	}
	os.WriteFile(filepath.Join(root, "image.png"), []byte("image"), 0600)
	if _, e = Snapshot(stdcontext.Background(), filepath.Join(root, "image.png"), false); e == nil {
		t.Fatal("silently accepted unsupported image")
	}
}

func TestNativeReplayIsCountedOnce(t *testing.T) {
	native := []byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"result"}]}`)
	m := provider.Message{Role: "assistant", Content: "result", Calls: []provider.ToolCall{{ID: "c", Name: "read", Arguments: []byte(`{}`)}}, State: &provider.ReplayState{Provider: "openai", Model: "test", Version: 1, Items: []json.RawMessage{native}}}
	if got := Tokens([]provider.Message{m}); got != Estimate(string(native)) {
		t.Fatalf("native payload counted twice: %d", got)
	}
}
