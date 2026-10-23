package history

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"ttc/internal/llm"
	"ttc/internal/messagestream"
)

func testInspection(text string) *llm.InspectionDocument {
	return &llm.InspectionDocument{Sections: []llm.InspectionSection{{Title: "Content", Fields: []llm.InspectionField{{Name: "text", Value: llm.InspectionValue{Kind: "string", Text: text}}}}}}
}
func recordStream(t *testing.T, s *Store, session Session, turn, actor, conversation, payload string) []messagestream.Item {
	t.Helper()
	request, err := s.StartRequest(session.ID, turn, actor, "coding", session.Model)
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.RecordBoundary(session.ID, turn, actor, request, conversation, llm.BoundaryEvent{Attempt: 1, Direction: "request", Payload: json.RawMessage(payload), Inspection: testInspection(payload)})
	if err != nil {
		t.Fatal(err)
	}
	return items
}
func TestBoundaryPersistenceReplayInspectionAndIsolation(t *testing.T) {
	s, session, turn, request := historyFixture(t)
	ctx := context.Background()
	before, err := s.Messages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	save := func(attempt int, direction, raw, text string) messagestream.Item {
		t.Helper()
		items, err := s.RecordBoundary(session.ID, turn, "main", request, "context", llm.BoundaryEvent{Attempt: attempt, Direction: direction, Payload: json.RawMessage(raw), Inspection: testInspection(text)})
		if err != nil || len(items) != 1 {
			t.Fatal(items, err)
		}
		return items[0]
	}
	raw := "{ \"input\": [\"hello\"] }\n"
	header := save(1, "request", raw, "hello\nworld")
	response := save(1, "response", `{"text":"answer"}`, "the answer")
	outcome := save(1, "outcome", `{"state":"done"}`, "completed")
	if header.ID != response.ID || header.ID != outcome.ID || header.Revision >= response.Revision || response.Revision >= outcome.Revision {
		t.Fatal(header, response, outcome)
	}
	replay, err := s.StreamItems(ctx, session.ID)
	if err != nil || !reflect.DeepEqual(replay, []messagestream.Item{outcome}) {
		t.Fatal(replay, outcome, err)
	}
	for _, id := range []int64{outcome.Revision} {
		inspection, err := s.ReadInspection(ctx, id)
		if err != nil || !strings.Contains(inspection.Text, "hello\n") || !strings.Contains(inspection.Text, "the answer") || !strings.Contains(inspection.Text, "completed") {
			t.Fatal(inspection, err)
		}
	}
	captures, err := s.BoundaryCaptures(ctx, outcome.Revision)
	if err != nil || len(captures) != 3 {
		t.Fatal(captures, err)
	}
	for i, want := range []string{raw, `{"text":"answer"}`, `{"state":"done"}`} {
		got, err := s.BoundaryRaw(ctx, captures[i].ID)
		if err != nil || string(got) != want {
			t.Fatal(string(got), err)
		}
	}
	got, err := s.BoundaryRaw(ctx, header.ID)
	if err != nil || string(got) != raw {
		t.Fatal(string(got), err)
	}
	after, err := s.Messages(session.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("capture entered context", err)
	}
	transcript, err := s.Transcript(session.ID, 0)
	if err != nil || bytes.Contains(transcript, []byte("presentation_stream")) {
		t.Fatal(string(transcript), err)
	}
	loaded, err := s.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := s.StreamItems(ctx, loaded.ID)
	if err != nil || len(copied) != len(replay) || copied[0].Title != replay[0].Title {
		t.Fatal(copied, err)
	}
	// Interrupted attempts remain distinct from retries, even on one request.
	retry := save(2, "request", `{}`, "retry")
	replay, err = s.StreamItems(ctx, session.ID)
	if err != nil || len(replay) != 2 || replay[1].ID != retry.ID {
		t.Fatal(replay, err)
	}
}

func TestBoundarySelectedBranchAndConcurrentUpdates(t *testing.T) {
	s, session, turn, request := historyFixture(t)
	ctx := context.Background()
	items, err := s.RecordBoundary(session.ID, turn, "main", request, "context", llm.BoundaryEvent{Attempt: 1, Direction: "request", Payload: json.RawMessage(`{}`), Inspection: testInspection("request")})
	if err != nil {
		t.Fatal(err)
	}
	header := items[0]
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.RecordBoundary(session.ID, turn, "main", request, "context", llm.BoundaryEvent{Attempt: 1, Direction: "response", Payload: json.RawMessage(`{"text":"sibling"}`), Inspection: testInspection("sibling")})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	replay, err := s.StreamItems(ctx, session.ID)
	if err != nil || len(replay) != 1 {
		t.Fatal(replay, err)
	}
	captures, err := s.BoundaryCaptures(ctx, replay[0].Revision)
	if err != nil || len(captures) != 9 {
		t.Fatal(captures, err)
	}
	if _, err := s.db.Exec("UPDATE sessions SET active_entry_id=? WHERE id=?", header.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	branch, err := s.RecordBoundary(session.ID, turn, "main", request, "context", llm.BoundaryEvent{Attempt: 1, Direction: "outcome", Payload: json.RawMessage(`{}`), Inspection: testInspection("new branch")})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := s.ReadInspection(ctx, branch[0].Revision)
	if err != nil || strings.Contains(inspection.Text, "sibling") || !strings.Contains(inspection.Text, "new branch") {
		t.Fatal(inspection, err)
	}
	captures, err = s.BoundaryCaptures(ctx, branch[0].Revision)
	if err != nil || len(captures) != 2 {
		t.Fatal(captures, err)
	}
}
func TestBoundaryArtifactsChunkDedupAndExactBytes(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	rng := rand.New(rand.NewSource(123))
	data := make([]byte, 2<<20)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	for i := range data {
		data[i] = alphabet[rng.Intn(len(alphabet))]
	}
	one := `{"instructions":"a","input":[{"file_data":"` + string(data) + `"}]}`
	two := strings.Replace(one, `"a"`, `"changed-length"`, 1)
	first := recordStream(t, s, session, turn, "main", "context", one)
	directory := filepath.Join(s.Root, "lineages", session.LineageID, "boundarychunks")
	initial, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	second := recordStream(t, s, session, turn, "main", "context", two)
	final, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(final) > len(initial)+10 {
		t.Fatal("chunks not shared", len(initial), len(final))
	}
	for i, want := range []string{one, two} {
		id := first[0].ID
		if i == 1 {
			id = second[0].ID
		}
		raw, err := s.BoundaryRaw(context.Background(), id)
		if err != nil || string(raw) != want {
			t.Fatal(len(raw), err)
		}
	}
}
func TestBoundaryAuditSurvivesCompaction(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	first := recordStream(t, s, session, turn, "main", "context", `{"input":["old"]}`)
	cut, err := s.Session(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := s.Continue(session.ID, llm.NewCompactionContext("summary"), cut.EntryTip, cut.EntryTip+1, nil, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.StreamItems(context.Background(), continued.ID)
	if err != nil || len(replay) != 1 || replay[0].Title != first[0].Title {
		t.Fatal(replay, err)
	}
	recordStream(t, s, continued, turn, "main", "compacted", `{"input":["summary"]}`)
	replay, err = s.StreamItems(context.Background(), continued.ID)
	if err != nil || len(replay) != 2 {
		t.Fatal(replay, err)
	}
}
func TestBoundaryOwnershipAndValidation(t *testing.T) {
	s, session, turn, request := historyFixture(t)
	for _, event := range []llm.BoundaryEvent{{Attempt: 1, Direction: "invalid", Payload: json.RawMessage(`{}`)}, {Direction: "request", Payload: json.RawMessage(`{}`)}} {
		if _, err := s.PrepareBoundary(session.ID, request, event); err == nil {
			t.Fatal("accepted invalid event")
		}
	}
	if _, err := s.RecordBoundary(session.ID, turn, "other", request, "context", llm.BoundaryEvent{Attempt: 1, Direction: "request", Payload: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("accepted actor mismatch")
	}
	if _, err := s.RecordBoundary(session.ID, turn, "main", request, "context", llm.BoundaryEvent{Attempt: 1, Direction: "response", Payload: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("accepted orphan response")
	}
}

func TestLegacyBoundaryArtifactsGroupWithoutNativeReparse(t *testing.T) {
	s, session, turn, request := historyFixture(t)
	var header int64
	for _, direction := range []string{"request", "change", "response", "outcome"} {
		raw, err := s.boundaryArtifact(session.ID, []byte(`{"original":"`+direction+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		readable, err := s.boundaryArtifact(session.ID, []byte("saved legacy "+direction))
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(boundaryRecord{Type: "presentation_stream", Direction: direction, Request: request, Attempt: 1, Raw: raw, Readable: readable, Item: messagestream.Item{Title: "Legacy request"}})
		if err != nil {
			t.Fatal(err)
		}
		err = s.transact(func(tx *sql.Tx) error {
			id, err := appendTx(tx, session.ID, turn, "main", "status", "", false, encoded, 0)
			if direction == "request" {
				header = id
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.StreamItems(context.Background(), session.ID)
	if err != nil || len(items) != 1 || items[0].ID != header {
		t.Fatal(items, err)
	}
	page, err := s.InspectPage(context.Background(), items[0].Actions[0].Target, 0, InspectionPageChars)
	if err != nil || page.Structured || !strings.Contains(page.Text, "saved legacy response") || strings.Contains(page.Text, "saved legacy change") || strings.Contains(page.Text, `"original"`) {
		t.Fatal(page, err)
	}
	expanded, err := s.BoundaryExpandedPage(context.Background(), items[0].Revision, 0, InspectionPageChars)
	if err != nil || expanded.Structured || expanded.Text != page.Text {
		t.Fatal(expanded, err)
	}
}

func TestBoundaryCompactExpandedAndImmutableHeaderSnapshots(t *testing.T) {
	s, session, turn, request := historyFixture(t)
	ctx := context.Background()
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d: ordinary full readable instructions", i)
	}
	lines[20] = "middle-only-expanded-marker"
	doc := testInspection(strings.Join(lines, "\n"))
	raw := "{ \"original\": \"request\" }\n"
	items, err := s.RecordBoundary(session.ID, turn, "main", request, "context", llm.BoundaryEvent{Attempt: 1, Direction: "request", Payload: json.RawMessage(raw), Inspection: doc})
	if err != nil {
		t.Fatal(err)
	}
	header := items[0]
	initial, err := s.InspectPage(ctx, header.Revision, 0, InspectionPageChars)
	if err != nil || strings.Contains(initial.Text, lines[20]) {
		t.Fatal(initial, err)
	}
	// A loaded header-only fork must never acquire later source-session details.
	fork, err := s.Load(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	response, err := s.RecordBoundary(session.ID, turn, "main", request, "context", llm.BoundaryEvent{Attempt: 1, Direction: "response", Payload: json.RawMessage(`{"response":true}`), Inspection: testInspection("source branch response")})
	if err != nil {
		t.Fatal(err)
	}
	outcomeDoc := &llm.InspectionDocument{State: "failed", Sections: []llm.InspectionSection{{Title: "Outcome", Fields: []llm.InspectionField{{Name: "state", Value: llm.InspectionValue{Kind: "string", Text: "failed"}}}}}}
	outcome, err := s.RecordBoundary(session.ID, turn, "main", request, "context", llm.BoundaryEvent{Attempt: 1, Direction: "outcome", Payload: json.RawMessage(`{"state":"failed"}`), Inspection: outcomeDoc})
	if err != nil || !strings.HasSuffix(outcome[0].Title, " · failed") {
		t.Fatal(outcome, err)
	}
	compact, err := s.InspectPage(ctx, outcome[0].Revision, 0, InspectionPageChars)
	if err != nil || strings.Contains(compact.Text, lines[20]) || strings.Count(strings.Split(compact.Text, "\n\nPrettified raw JSON")[0], "Outcome\n") != 1 {
		t.Fatal(compact, err)
	}
	expanded, err := s.BoundaryExpandedPage(ctx, outcome[0].Revision, 0, InspectionPageChars)
	if err != nil || !expanded.Structured || !strings.Contains(expanded.Text, lines[20]) || !strings.Contains(expanded.Text, "source branch response") || strings.Count(strings.Split(expanded.Text, "\n\nPrettified raw JSON")[0], "Outcome\n") != 1 || expanded.Total <= compact.Total {
		t.Fatal(expanded, err)
	}
	for _, id := range []int64{header.Revision, response[0].Revision} {
		captures, err := s.BoundaryCaptures(ctx, id)
		want := 1
		if id == response[0].Revision {
			want = 2
		}
		if err != nil || len(captures) != want {
			t.Fatal(captures, err)
		}
	}
	after, err := s.InspectPage(ctx, header.Revision, 0, InspectionPageChars)
	if err != nil || after.Text != initial.Text || after.Total != initial.Total {
		t.Fatal(after, initial, err)
	}
	forkItems, err := s.StreamItems(ctx, fork.ID)
	if err != nil || len(forkItems) != 1 {
		t.Fatal(forkItems, err)
	}
	forkPage, err := s.BoundaryExpandedPage(ctx, forkItems[0].Revision, 0, InspectionPageChars)
	if err != nil || strings.Contains(forkPage.Text, "source branch response") || !strings.Contains(forkPage.Text, lines[20]) {
		t.Fatal(forkPage, err)
	}
	got, err := s.BoundaryRaw(ctx, header.ID)
	if err != nil || string(got) != raw {
		t.Fatal(string(got), err)
	}
	// A response lookup reaches just itself and its header, not older history.
	var visited int
	if err := s.db.QueryRowContext(ctx, boundaryAttemptAncestry+`SELECT count(*) FROM ancestry`, request, 1, 0, response[0].Revision).Scan(&visited); err != nil || visited != 2 {
		t.Fatal(visited, err)
	}
}

func TestBoundaryLayoutCacheBounds(t *testing.T) {
	var cache boundaryLayoutCache
	for id := int64(1); id <= 40; id++ {
		cache.put(boundaryLayoutKey{id: id}, &boundaryLayout{parts: make([]boundaryLayoutPart, 1)})
	}
	if len(cache.entries) != 32 || cache.parts != 32 || cache.get(boundaryLayoutKey{id: 1}) != nil {
		t.Fatal(len(cache.entries), cache.parts)
	}
	for id := int64(41); id <= 50; id++ {
		cache.put(boundaryLayoutKey{id: id}, &boundaryLayout{parts: make([]boundaryLayoutPart, 10000)})
	}
	if len(cache.entries) > 32 || cache.parts > 65536 {
		t.Fatal(len(cache.entries), cache.parts)
	}
	cache.put(boundaryLayoutKey{id: 100}, &boundaryLayout{parts: make([]boundaryLayoutPart, 65537)})
	if cache.get(boundaryLayoutKey{id: 100}) != nil {
		t.Fatal("oversized layout cached")
	}
}

func TestBoundaryReadableThenPrettifiedJSON(t *testing.T) {
	s, session, turn, request := historyFixture(t)
	ctx := context.Background()
	raws := []string{
		"{ \"instructions\": \"native request\", \"input\": [1e+09, true, null, {\"content\":\"world 🌍\"}] }\n",
		`{"output":[{"text":"native response"}]}`,
		`{"state":"completed","usage":{"tokens":42}}`,
	}
	var latest messagestream.Item
	for i, direction := range []string{"request", "response", "outcome"} {
		items, err := s.RecordBoundary(session.ID, turn, "main", request, "context", llm.BoundaryEvent{Attempt: 1, Direction: direction, Payload: json.RawMessage(raws[i]), Inspection: testInspection("human-readable " + direction)})
		if err != nil {
			t.Fatal(err)
		}
		latest = items[0]
	}
	compact, err := s.InspectPage(ctx, latest.Revision, 0, InspectionPageChars)
	if err != nil {
		t.Fatal(err)
	}
	expanded, err := s.BoundaryExpandedPage(ctx, latest.Revision, 0, InspectionPageChars)
	if err != nil {
		t.Fatal(err)
	}
	captures, err := s.BoundaryCaptures(ctx, latest.Revision)
	if err != nil || len(captures) != 3 {
		t.Fatal(captures, err)
	}
	for _, page := range []InspectionPage{compact, expanded} {
		boundary := strings.Index(page.Text, "\n\nPrettified raw JSON")
		if boundary < 0 {
			t.Fatal("missing same-window JSON section", page.Text)
		}
		for _, direction := range []string{"request", "response", "outcome"} {
			at := strings.Index(page.Text, "human-readable "+direction)
			if at < 0 || at >= boundary {
				t.Fatal("human view must precede all JSON", page.Text)
			}
		}
		if !strings.Contains(page.Text[boundary:], "Request\n{\n  \"instructions\":") || !strings.Contains(page.Text[boundary:], "Response #1\n{\n  \"output\":") || !strings.Contains(page.Text[boundary:], "Outcome\n{\n  \"state\":") {
			t.Fatal("native JSON lost standard object syntax", page.Text)
		}
	}
	for i, capture := range captures {
		pretty, err := readBoundaryAll(ctx, capture.prettyJSON)
		if err != nil {
			t.Fatal(err)
		}
		decode := func(data string) any {
			t.Helper()
			var value any
			decoder := json.NewDecoder(strings.NewReader(data))
			decoder.UseNumber()
			if err := decoder.Decode(&value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		if !reflect.DeepEqual(decode(string(pretty)), decode(raws[i])) {
			t.Fatal("formatting changed JSON semantics", string(pretty))
		}
		original, err := s.BoundaryRaw(ctx, capture.ID)
		if err != nil || string(original) != raws[i] {
			t.Fatal("raw bytes changed", string(original), err)
		}
	}
	inspection, err := s.ReadInspection(ctx, latest.Revision)
	if err != nil || inspection.Text != compact.Text {
		t.Fatal("small and paged inspectors disagree", inspection, err)
	}
}

func TestBoundaryMalformedJSONIsNonfatalAndIndentationIsBounded(t *testing.T) {
	s, session, turn, request := historyFixture(t)
	raw := "{ malformed original bytes\n"
	items, err := s.RecordBoundary(session.ID, turn, "main", request, "context", llm.BoundaryEvent{Attempt: 1, Direction: "request", Payload: json.RawMessage(raw), Inspection: testInspection("readable still available")})
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	for _, expanded := range []bool{false, true} {
		text, _, err := s.boundaryReadablePage(context.Background(), items[0].Revision, 0, InspectionPageChars, expanded)
		if err != nil || !strings.Contains(text, "readable still available") || !strings.Contains(text, "original capture is not valid JSON") {
			t.Fatal(text, err)
		}
	}
	got, err := s.BoundaryRaw(context.Background(), items[0].ID)
	if err != nil || string(got) != raw {
		t.Fatal(string(got), err)
	}
	nested := []byte(strings.Repeat("[", 50) + "0" + strings.Repeat("]", 50))
	if !json.Valid(nested) {
		t.Fatal("invalid size-limit fixture")
	}
	pretty := prettyBoundaryJSON(nested, 256)
	if !strings.Contains(string(pretty), "exceeds the size limit") || len(pretty) > 256 {
		t.Fatal(string(pretty))
	}
}

func TestBoundaryAggregateJSONOverflowPreservesHumanView(t *testing.T) {
	s, session, turn, request := historyFixture(t)
	ctx := context.Background()
	var latest messagestream.Item
	for _, direction := range []string{"request", "response"} {
		raw := `{"native":"` + strings.Repeat(direction, 140) + `"}`
		items, err := s.RecordBoundary(session.ID, turn, "main", request, "context", llm.BoundaryEvent{Attempt: 1, Direction: direction, Payload: json.RawMessage(raw), Inspection: testInspection(strings.Repeat("ordinary readable "+direction+" text ", 30))})
		if err != nil {
			t.Fatal(err)
		}
		latest = items[0]
	}
	details, err := s.BoundaryCaptures(ctx, latest.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, expanded := range []bool{false, true} {
		sources := boundaryReadableSources(details, expanded)
		humanLayout, err := buildBoundaryLayout(ctx, sources[:len(details)], 4096)
		if err != nil {
			t.Fatal(err)
		}
		human, err := humanLayout.all(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// The source documents fit individually, but their combined JSON exceeds
		// the remaining budget. Neither compact nor expanded human content may
		// disappear because an optional second representation is too large.
		layout, err := buildBoundaryLayout(ctx, sources, 2048)
		if err != nil {
			t.Fatal(expanded, err)
		}
		all, err := layout.all(ctx)
		if err != nil || !strings.HasPrefix(all, human) || strings.Count(all, "Remaining prettified JSON omitted") != 1 {
			t.Fatal(expanded, all, err)
		}
		wantJSON := 1
		if expanded {
			wantJSON = 0
		}
		if strings.Count(all, `"native"`) != wantJSON {
			t.Fatal("JSON documents should be included whole until budget is exhausted", expanded, all)
		}
		var paged strings.Builder
		for offset := 0; offset < layout.chars; offset += 37 {
			text, err := layout.page(ctx, offset, 37)
			if err != nil {
				t.Fatal(err)
			}
			paged.WriteString(text)
		}
		if paged.String() != all {
			t.Fatal("full and paged layout disagree")
		}
		// Exercise public readers with the same small-budget layout, avoiding
		// hundreds of MiB of test allocations while testing their shared policy.
		s.boundaryLayouts.put(boundaryLayoutKey{id: latest.Revision, expanded: expanded}, layout)
		var page InspectionPage
		if expanded {
			page, err = s.BoundaryExpandedPage(ctx, latest.Revision, 0, InspectionPageChars)
		} else {
			page, err = s.InspectPage(ctx, latest.Revision, 0, InspectionPageChars)
		}
		if err != nil || page.Text != all {
			t.Fatal(page, err)
		}
		if !expanded {
			inspection, err := s.ReadInspection(ctx, latest.Revision)
			if err != nil || inspection.Text != all {
				t.Fatal(inspection, err)
			}
		}
	}
}

func TestBoundaryFullHumanBudgetStillGetsJSONOmissionNotice(t *testing.T) {
	ctx := context.Background()
	human := "the entire human-readable body"
	layout, err := buildBoundaryLayout(ctx, []boundaryReadableSource{{text: human}, {text: "\n\nRequest\n{}", json: true}}, int64(len(human)))
	if err != nil {
		t.Fatal(err)
	}
	all, err := layout.all(ctx)
	if err != nil || all != human+"\n\nPrettified raw JSON"+boundaryJSONOmission {
		t.Fatal(all, err)
	}
	if _, err := buildBoundaryLayout(ctx, []boundaryReadableSource{{text: human}}, int64(len(human)-1)); err == nil {
		t.Fatal("human content exceeded its existing body limit")
	}
}
