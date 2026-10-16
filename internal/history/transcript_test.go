package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
)

func transcriptEventIDs(t *testing.T, data []byte) []int64 {
	t.Helper()
	var ids []int64
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var row struct {
			Seq int64 `json:"event_seq"`
		}
		err := decoder.Decode(&row)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, row.Seq)
	}
	return ids
}

func TestActorTranscriptMainAncestryFrozenCutAndLoadCopies(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	add := func(session, actor, text string) int64 {
		t.Helper()
		id, err := s.Append(session, turn, actor, "message", "assistant", actor == "main", llm.Message{Type: llm.TypeAssistant, Content: text})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	old := add(v.ID, "main", "Original main work")
	cut := add(v.ID, "main", "Frozen main cut")
	sibling := add(v.ID, "main/child_other", "Never export sibling")
	late := add(v.ID, "main", "Later arrival retained but not summarized")
	next, err := s.Continue(v.ID, "Selected summary", cut, cut, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	latestSession, latestTip, err := s.ActorCompactionTip(context.Background(), next.ID, "main", next.EntryTip)
	if err != nil || latestSession != v.ID || latestTip != cut {
		t.Fatal("source cut moved at handoff", latestSession, latestTip, err)
	}
	data, err := s.ActorTranscriptJSONL(latestSession, "main", latestTip)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("Later arrival")) {
		t.Fatal("before_compaction included a later commit")
	}
	if _, err := s.Load(next.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(next.ID)
	if err != nil {
		t.Fatal(err)
	}
	loadedAgain, err := s.Load(loaded.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []Session{next, loaded, loadedAgain} {
		data, err := s.ActorTranscriptJSONL(session.ID, "main", session.EntryTip)
		if err != nil {
			t.Fatal(err)
		}
		ids := transcriptEventIDs(t, data)
		counts := map[int64]int{}
		for _, id := range ids {
			counts[id]++
		}
		for _, id := range []int64{old, cut, late} {
			if counts[id] != 1 {
				t.Fatal("lost/duplicated source", session.ID, id, ids)
			}
		}
		if counts[sibling] != 0 || bytes.Contains(data, []byte("Never export sibling")) {
			t.Fatal("sibling exported", string(data))
		}
		if gotSession, gotTip, err := s.ActorCompactionTip(context.Background(), session.ID, "main", session.EntryTip); err != nil || gotSession != v.ID || gotTip != cut {
			t.Fatal(gotSession, gotTip, err)
		}
	}
	if _, err := s.ActorTranscript(v.ID, "main", next.EntryTip); err == nil {
		t.Fatal("foreign session tip accepted")
	}
	empty, err := s.ActorTranscriptJSONL(next.ID, "main", 0)
	if err != nil || len(empty) != 0 {
		t.Fatal("zero did not freeze empty branch", string(empty), err)
	}
}

func TestActorTranscriptMultipleCompactionsAcrossLoadCopies(t *testing.T) {
	s, original, _, _ := historyFixture(t)
	appendMessage := func(session, text string) int64 {
		t.Helper()
		id, err := s.Append(session, "", "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: text})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := appendMessage(original.ID, "First original evidence")
	firstCut := appendMessage(original.ID, "First frozen cut")
	continued, err := s.Continue(original.ID, "First handoff", firstCut, firstCut, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(continued.ID)
	if err != nil {
		t.Fatal(err)
	}
	second := appendMessage(loaded.ID, "Second original evidence")
	secondCut := appendMessage(loaded.ID, "Second frozen cut")
	late := appendMessage(loaded.ID, "After second frozen cut")
	continuedAgain, err := s.Continue(loaded.ID, "Second handoff", secondCut, secondCut, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	loadedAgain, err := s.Load(continuedAgain.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := s.ActorTranscriptJSONL(loadedAgain.ID, "main", loadedAgain.EntryTip)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[int64]int{}
	for _, id := range transcriptEventIDs(t, data) {
		counts[id]++
	}
	for _, id := range []int64{first, firstCut, second, secondCut, late} {
		if counts[id] != 1 {
			t.Fatalf("original event %d appears %d times", id, counts[id])
		}
	}
	source, tip, err := s.ActorCompactionTip(context.Background(), loadedAgain.ID, "main", loadedAgain.EntryTip)
	if err != nil || source != loaded.ID || tip != secondCut {
		t.Fatal("latest cut lost across copied summaries", source, tip, err)
	}
	data, err = s.ActorTranscriptJSONL(source, "main", tip)
	if err != nil || bytes.Contains(data, []byte("After second frozen cut")) || !bytes.Contains(data, []byte("First original evidence")) {
		t.Fatal("precompaction export changed its frozen cut or lost ancestry", string(data), err)
	}
}

func TestActorTranscriptAsidePrefixAndActorCompaction(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	prefix, err := s.Append(v.ID, turn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "Admitted main prefix"})
	if err != nil {
		t.Fatal(err)
	}
	actor := "main/aside_test"
	if err := s.RecordActorPrefix(v.ID, turn, actor, v.ID, prefix); err != nil {
		t.Fatal(err)
	}
	own, err := s.Append(v.ID, turn, actor, "message", "user", false, llm.Message{Type: llm.TypeUserInput, Content: "Aside question"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordActorCompaction(v.ID, turn, actor, v.ID, own); err != nil {
		t.Fatal(err)
	}
	last, err := s.Append(v.ID, turn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "Not in frozen prefix"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := s.ActorTranscript(v.ID, actor, last)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Admitted main prefix", "Aside question"} {
		if !bytes.Contains(data, []byte(want)) {
			t.Fatal("missing aside context", want, string(data))
		}
	}
	if bytes.Contains(data, []byte("Not in frozen prefix")) {
		t.Fatal("prefix was not frozen", string(data))
	}
	source, tip, err := s.ActorCompactionTip(context.Background(), v.ID, actor, last)
	if err != nil || source != v.ID || tip != own {
		t.Fatal(source, tip, err)
	}
	loaded, err := s.Load(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err = s.ActorTranscript(loaded.ID, actor, loaded.EntryTip)
	if err != nil || !bytes.Contains(data, []byte("Admitted main prefix")) {
		t.Fatal(string(data), err)
	}
}

type blockingTranscriptWriter struct {
	entered chan struct{}
	release chan struct{}
}

func (w *blockingTranscriptWriter) Write(p []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	return len(p), nil
}

func TestSlowTranscriptWriterDoesNotBlockCommits(t *testing.T) {
	s, v, turn, _ := historyFixture(t)
	w := &blockingTranscriptWriter{make(chan struct{}, 1), make(chan struct{})}
	finished := make(chan error, 1)
	go func() {
		finished <- s.WriteActorTranscript(context.Background(), w, v.ID, "main", v.EntryTip, "markdown")
	}()
	defer func() {
		close(w.release)
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("export did not reach writer")
	}
	committed := make(chan error, 1)
	go func() {
		_, err := s.Append(v.ID, turn, "main", "message", "assistant", true, llm.Message{Type: llm.TypeAssistant, Content: "Commit while export writer waits"})
		committed <- err
	}()
	select {
	case err := <-committed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow output writer blocked history commits")
	}
}

func TestCompactionSummaryLookupUsesIndex(t *testing.T) {
	s, _, _, _ := historyFixture(t)
	rows, err := s.db.Query("EXPLAIN QUERY PLAN SELECT predecessor_id,source_tip_id FROM compactions WHERE summary_entry_id=?", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "compaction_summary") {
		t.Fatal("summary ancestry lookup scans compactions", plan.String())
	}
}

func TestWriteActorTranscriptCancellationAndFormats(t *testing.T) {
	s, v, _, _ := historyFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if err := s.WriteActorTranscript(ctx, &out, v.ID, "main", v.EntryTip, "markdown"); !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatal(err, out.String())
	}
	if err := s.WriteActorTranscript(context.Background(), &out, v.ID, "main", v.EntryTip, "other"); err == nil {
		t.Fatal("unknown format accepted")
	}
	for _, format := range []string{"markdown", "jsonl"} {
		out.Reset()
		if err := s.WriteActorTranscript(context.Background(), &out, v.ID, "main", v.EntryTip, format); err != nil || out.Len() == 0 {
			t.Fatal(format, err)
		}
	}
	if _, _, err := s.ActorCompactionTip(context.Background(), v.ID, "main", v.EntryTip); err == nil {
		t.Fatal(err)
	}
}
