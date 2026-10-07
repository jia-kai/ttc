package session

import (
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"ttc/internal/llm"
	"ttc/internal/tool"
)

func showTestImage(t *testing.T, r *Runtime, id, actor string, click bool) ImageSnapshot {
	t.Helper()
	r.mu.Lock()
	persisted := r.persisted
	r.mu.Unlock()
	if !persisted {
		seedRuntime(t, r, "Display the research image")
	}
	path := filepath.Join(r.Workspace.Root, "field.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(f, image.NewNRGBA(image.Rect(0, 0, 20, 10))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	args, _ := json.Marshal(map[string]any{"path": "field.png", "request_click": click})
	record := r.Tools.Invoke(context.Background(), tool.Execution{SessionID: r.Current(), Actor: actor, CallID: id}, "image_show", args)
	var v ImageSnapshot
	if err = json.Unmarshal(record.Result, &v); err != nil || v.ID == "" {
		t.Fatal(string(record.Result), err)
	}
	return v
}

func TestChildImageEventRoutesToCurrentContinuation(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	r.EnableImageClicks(true)
	showTestImage(t, r, "fixture", "main", false)
	old := r.Current()
	_, ids := batchIntents(t, r, "main/child", []llm.ToolCall{{ID: "old_child_call", Name: "image_show", Arguments: json.RawMessage(`{"path":"field.png","request_click":true}`)}})
	for _, text := range []string{strings.Repeat("old research ", 3000), "Continue the child task."} {
		if _, err := r.Store.Append(old, "", "main", "message", "user", true, llm.Message{Role: "user", Content: text}); err != nil {
			t.Fatal(err)
		}
	}
	r.Provider = &llm.Script{Responses: []llm.ScriptResponse{{Text: "Continue the active child and its pending display."}}}
	if _, err := r.Command("/compact"); err != nil {
		t.Fatal(err)
	}
	record := r.Tools.Invoke(context.Background(), tool.Execution{SessionID: old, Actor: "main/child", CallID: ids[0]}, "image_show", json.RawMessage(`{"path":"field.png","request_click":true}`))
	if strings.Contains(string(record.Result), `"error"`) {
		t.Fatal(string(record.Result))
	}
	found := false
	for len(events) > 0 {
		e := <-events
		if e.Kind == "image" && e.CallID == ids[0] {
			found = true
			if e.SessionID != r.Current() {
				t.Fatal("child image routed to predecessor", e.SessionID)
			}
		}
	}
	if !found {
		t.Fatal("missing published image")
	}
	cards, err := r.PendingImages()
	if err != nil || len(cards) != 1 || cards[0].EntryID == 0 {
		t.Fatal("pending child intent not reachable", cards, err)
	}
	image, err := r.ImageForEntry(cards[0].EntryID)
	if err != nil || image == nil || image.ID != ids[0] {
		t.Fatal("pending intent cannot open preview", image, err)
	}
}

func TestChildImageReplySlotAndExitCleanup(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.EnableImageClicks(true)
	v := showTestImage(t, r, "one", "main/child", true)
	if err := r.ConfirmImage(v.ID, &[2]int{1, 2}); err != nil {
		t.Fatal(err)
	}
	record := r.Tools.Invoke(context.Background(), tool.Execution{SessionID: r.Current(), Actor: "main/child", CallID: "two"}, "image_show", json.RawMessage(`{"path":"field.png","request_click":true}`))
	if !strings.Contains(string(record.Result), "click_already_pending") {
		t.Fatal("unconsumed reply overwritten", string(record.Result))
	}
	m, err := r.childImageReply(context.Background(), "main/child", false)
	if err != nil || m == nil || !strings.Contains(m.Content, `"image_id":"one"`) {
		t.Fatal(m, err)
	}
	v = showTestImage(t, r, "three", "main/child", true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.runChild(ctx, childTask{actor: "main/child", prompt: "task", selection: r.CurrentSelection(), tools: r.Tools}, io.Discard, io.Discard); err == nil {
		t.Fatal("canceled child continued")
	}
	if r.ImageClickPending(v.ID) {
		t.Fatal("exited child left interaction armed")
	}
	m, err = r.childImageReply(context.Background(), "main/child", false)
	if err != nil || m != nil {
		t.Fatal("exited child left reply", m, err)
	}
}

func TestImageFailedAdmissionReleasesActor(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.EnableImageClicks(true)
	record := r.Tools.Invoke(context.Background(), tool.Execution{SessionID: r.Current(), Actor: "main", CallID: "missing"}, "image_show", json.RawMessage(`{"path":"missing.png","request_click":true}`))
	if !strings.Contains(string(record.Result), "error") {
		t.Fatal(string(record.Result))
	}
	v := showTestImage(t, r, "valid", "main", true)
	if !r.ImageClickPending(v.ID) {
		t.Fatal("failed admission retained reservation")
	}
}
func TestImageImmutableSnapshotAndPendingLifecycle(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.EnableImageClicks(true)
	v := showTestImage(t, r, "image1", "main", true)
	if err := os.Remove(v.Path); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(v.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := image.DecodeConfig(f)
	f.Close()
	if err != nil || c.Width != 20 || c.Height != 10 {
		t.Fatal(c, err)
	}
	record := r.Tools.Invoke(context.Background(), tool.Execution{SessionID: r.Current(), Actor: "main", CallID: "image2"}, "image_show", json.RawMessage(`{"path":"field.png","request_click":true}`))
	if !strings.Contains(string(record.Result), "click_already_pending") {
		t.Fatal(string(record.Result))
	}
	if err = r.ConfirmImage(v.ID, &[2]int{20, 0}); err == nil {
		t.Fatal("out of bounds coordinate")
	}
	if !r.ImageClickPending(v.ID) {
		t.Fatal("invalid coordinate consumed pending request")
	}
	if err = r.ConfirmImage(v.ID, &[2]int{3, 4}); err != nil {
		t.Fatal(err)
	}
	if err = r.ConfirmImage(v.ID, nil); err == nil {
		t.Fatal("duplicate completion")
	}
	if !r.HasNotifications() {
		t.Fatal("model not notified")
	}
	r.mu.Lock()
	content := r.notifications[0].Content
	r.mu.Unlock()
	if !strings.Contains(content, `"x":3`) || !strings.Contains(content, `"precision":"cell"`) {
		t.Fatal(content)
	}
	v = showTestImage(t, r, "image3", "main", true)
	if _, err = r.Command("/new"); err != nil {
		t.Fatal(err)
	}
	if r.ImageClickPending(v.ID) || r.HasNotifications() {
		t.Fatal("new session retained interaction")
	}
}
func TestImageChildRoutingAndUnsupportedPlain(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	record := r.Tools.Invoke(context.Background(), tool.Execution{SessionID: r.Current(), Actor: "main", CallID: "bad"}, "image_show", json.RawMessage(`{"path":"missing.png","request_click":true}`))
	if !strings.Contains(string(record.Result), "unsupported_interaction") {
		t.Fatal(string(record.Result))
	}
	r.EnableImageClicks(true)
	v := showTestImage(t, r, "child-image", "main/child", true)
	if err := r.ConfirmImage(v.ID, nil); err != nil {
		t.Fatal(err)
	}
	m, err := r.childImageReply(context.Background(), "main/child", true)
	if err != nil || m == nil || !strings.Contains(m.Content, "image_click_cancelled") {
		t.Fatal(m, err)
	}
	if r.HasNotifications() {
		t.Fatal("child reply leaked to parent")
	}
	if err = r.ConfirmImage("ordinary", nil); err == nil {
		t.Fatal("ordinary preview emitted cancellation")
	}
}
func TestContextUsageMatchesFitAndFrozenModel(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	s := r.CurrentSelection()
	defs := r.Tools.Definitions()
	u := estimateUsage(s, "instructions", defs, nil)
	sum := 0
	for _, p := range u.Parts {
		sum += p.Tokens
	}
	if sum != u.Input+u.Reserved {
		t.Fatal("categories overlap", u)
	}
	if u.Model != s.Model.ID+" · "+s.Variant {
		t.Fatal(u)
	}
	r.mu.Lock()
	r.usage = u
	r.mu.Unlock()
	copy := r.UsageSnapshot()
	copy.Parts[0].Tokens = 0
	if r.UsageSnapshot().Parts[0].Tokens == 0 {
		t.Fatal("snapshot does not own slices")
	}
}

func TestImagePendingSurvivesCompactionButNeverReplay(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Provider = &llm.Script{Responses: []llm.ScriptResponse{{Text: "Preserve the displayed image and pending click."}}}
	r.EnableImageClicks(true)
	v := showTestImage(t, r, "pending", "main", true)
	for _, text := range []string{strings.Repeat("old research ", 700), "recent question"} {
		if _, err := r.Store.Append(r.Current(), "", "main", "message", "user", true, llm.Message{Role: "user", Content: text}); err != nil {
			t.Fatal(err)
		}
	}
	before := r.Current()
	generation := r.Generation()
	if _, err := r.Command("/compact"); err != nil {
		t.Fatal(err)
	}
	if before == r.Current() || !r.ImageClickPending(v.ID) || r.Generation() != generation {
		t.Fatal("compaction lost live request")
	}
	if err := r.ConfirmImage(v.ID, &[2]int{1, 2}); err != nil {
		t.Fatal(err)
	}
	if !r.HasNotifications() {
		t.Fatal("compaction lost routing")
	}
	if _, err := r.Command("/load " + before); err != nil {
		t.Fatal(err)
	}
	if r.ImageClickPending(v.ID) || r.HasNotifications() || r.Generation() == generation {
		t.Fatal("history revived pending input")
	}
}
