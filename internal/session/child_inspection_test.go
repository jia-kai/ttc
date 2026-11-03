package session

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"ttc/internal/jobs"
	"ttc/internal/llm"
)

func childInspectionValues(spec jobs.InspectionSpec) map[string]string {
	values := make(map[string]string, len(spec.Fields))
	for _, field := range spec.Fields {
		values[field.Name] = field.Value
	}
	return values
}

func TestChildPromptInspectionInitialRecentAndImmutable(t *testing.T) {
	cache := newChildPromptInspection("original assignment")
	view := jobs.Snapshot{Label: "research"}
	initial := cache.inspect(view)
	if got := childInspectionValues(initial); len(got) != 2 || got["Label"] != "research" || got["Assignment prompt"] != "original assignment" {
		t.Fatal(got)
	}
	wantStreams := []jobs.InspectionStream{
		{Name: "stdout", Heading: "stdout · showing tail, up to 8 KiB", LimitBytes: 8192},
		{Name: "stderr", Heading: "stderr · showing tail, up to 8 KiB", LimitBytes: 8192},
	}
	if !reflect.DeepEqual(initial.Streams, wantStreams) {
		t.Fatal("default output inspection changed", initial.Streams)
	}
	cache.appendSteer("first accepted guidance")
	first := cache.inspect(view)
	cache.appendSteer("second accepted guidance")
	cache.appendSteer("third accepted guidance")
	got := childInspectionValues(cache.inspect(view))
	if len(got) != 5 || got["Assignment prompt"] != "original assignment" || got["Previous parent steering"] != "second accepted guidance" || got["Latest parent steering"] != "third accepted guidance" || !strings.Contains(got["Parent steering history"], "omitted") {
		t.Fatal(got)
	}
	if len(initial.Fields) != 2 || childInspectionValues(first)["Latest parent steering"] != "first accepted guidance" {
		t.Fatal("published snapshots were mutated", initial, first)
	}
	initial.Fields[1].Value = "caller mutation"
	if childInspectionValues(cache.inspect(view))["Assignment prompt"] != "original assignment" {
		t.Fatal("inspector returned shared mutable fields")
	}
}

func TestChildPromptInspectionTruncationUTF8AndBounds(t *testing.T) {
	for _, limit := range []int{childInspectionPromptBytes, childInspectionSteerBytes} {
		for _, text := range []string{
			"short text",
			strings.Repeat("x", limit),
			strings.Repeat("x", limit+1),
			strings.Repeat("界🙂", limit),
			strings.Repeat("x", limit-len(childInspectionTruncated)-1) + "界" + strings.Repeat("tail", limit),
			strings.Repeat("\xff", limit),
		} {
			got := childInspectionText(text, limit)
			if len(got) > limit || !utf8.ValidString(got) {
				t.Fatalf("invalid bounded UTF-8: limit=%d bytes=%d valid=%t", limit, len(got), utf8.ValidString(got))
			}
			if len(text) <= limit && utf8.ValidString(text) {
				if got != text {
					t.Fatal("untruncated text changed")
				}
			} else if !strings.HasSuffix(got, childInspectionTruncated) {
				t.Fatal("truncation was not disclosed")
			}
		}
		prefix := strings.Repeat("x", limit-len(childInspectionTruncated)-1)
		if got := childInspectionText(prefix+"界"+strings.Repeat("tail", limit), limit); got != prefix+childInspectionTruncated {
			t.Fatal("truncation split or replaced a partial UTF-8 rune")
		}
	}
	cache := newChildPromptInspection(strings.Repeat("🙂", 10000))
	for range 3 {
		cache.appendSteer(strings.Repeat("界", 10000))
	}
	snapshot := cache.snapshot.Load()
	if len(snapshot.initial) > childInspectionPromptBytes || snapshot.count != 2 || !snapshot.omitted {
		t.Fatal("prompt snapshot was not bounded")
	}
	for _, steer := range snapshot.steers {
		if len(steer) > childInspectionSteerBytes || !strings.HasSuffix(steer, childInspectionTruncated) {
			t.Fatal("steering snapshot was not bounded")
		}
	}
}

func TestChildPromptInspectionConcurrentReaders(t *testing.T) {
	cache := newChildPromptInspection("immutable original")
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			for range 1000 {
				spec := cache.inspect(jobs.Snapshot{Label: "concurrent"})
				values := childInspectionValues(spec)
				if values["Assignment prompt"] != "immutable original" || len(spec.Fields) > 5 {
					t.Error("inconsistent presentation snapshot", values)
					return
				}
			}
		})
	}
	// Accepted guidance writers are serialized by assignment admission; readers
	// do not participate in that lock or mutate the published snapshot.
	for i := range 1000 {
		cache.appendSteer(fmt.Sprintf("guidance %d 界", i))
	}
	readers.Wait()
	if got := childInspectionValues(cache.inspect(jobs.Snapshot{})); got["Latest parent steering"] != "guidance 999 界" {
		t.Fatal(got)
	}
}

func TestChildJobInspectionAcceptedGuidanceAndPersistentReuse(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.Emit = nil
	firstStarted, firstRelease := make(chan struct{}), make(chan struct{})
	secondStarted, secondRelease := make(chan struct{}), make(chan struct{})
	releaseFirst := sync.OnceFunc(func() { close(firstRelease) })
	releaseSecond := sync.OnceFunc(func() { close(secondRelease) })
	defer releaseFirst()
	defer releaseSecond()
	var phase atomic.Int32
	phase.Store(1)
	var firstBlocked, secondBlocked atomic.Bool
	r.Provider = &childProvider{stream: func(ctx context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		var release <-chan struct{}
		if phase.Load() == 1 && firstBlocked.CompareAndSwap(false, true) {
			close(firstStarted)
			release = firstRelease
		} else if phase.Load() == 2 && secondBlocked.CompareAndSwap(false, true) {
			close(secondStarted)
			release = secondRelease
		}
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return emit(llm.StreamEvent{Kind: "text", Text: "child output"})
	}}
	args := `{"persistent":true,"prompt":"first assignment prompt","label":"research","background":true}`
	turn, calls := batchIntents(t, r, "main", []llm.ToolCall{{ID: "spawn", Name: "subagent", Arguments: []byte(args)}})
	if _, err := r.Store.Append(r.Current(), turn, "main", "message", "user", true, llm.Message{Type: llm.TypeUserInput, Content: "private human main-session transcript"}); err != nil {
		t.Fatal(err)
	}
	first := childInvocation(t, r, calls[0], args)
	if first["ok"] != true {
		t.Fatal(first)
	}
	receive(t, firstStarted)
	firstJob := first["job_id"].(string)
	inspect := func(id string) jobs.Inspection {
		t.Helper()
		inspection, err := r.Jobs.Inspect(context.Background(), "main", id)
		if err != nil {
			t.Fatal(err)
		}
		return inspection
	}
	if got := childInspectionValues(inspect(firstJob).Spec); got["Assignment prompt"] != "first assignment prompt" || got["Label"] != "research" {
		t.Fatal("job did not install original prompt before launch", got)
	}
	for _, guidance := range []string{"oldest guidance", "previous guidance", "latest guidance"} {
		result := sendChildSteer(t, r, "main", calls[0], map[string]any{"job_id": firstJob, "message": guidance})
		if result["ok"] != true || result["state"] != "queued" {
			t.Fatal(result)
		}
	}
	for _, rejected := range []map[string]any{
		{"job_id": firstJob, "child_turn_id": "stale", "message": "rejected stale guidance"},
		{"job_id": firstJob, "message": strings.Repeat("rejected oversized guidance", 10000)},
	} {
		if result := sendChildSteer(t, r, "main", calls[0], rejected); result["ok"] == true {
			t.Fatal("invalid guidance accepted", result)
		}
	}
	if result := sendChildSteer(t, r, first["child_id"].(string), calls[0], map[string]any{"job_id": firstJob, "message": "rejected child guidance"}); result["ok"] == true {
		t.Fatal("nonparent guidance accepted", result)
	}
	live := inspect(firstJob)
	values := childInspectionValues(live.Spec)
	if values["Previous parent steering"] != "previous guidance" || values["Latest parent steering"] != "latest guidance" || len(values) != 5 {
		t.Fatal("live inspection missed accepted guidance or included rejected guidance", values)
	}
	detail := r.JobDetail(firstJob, "")
	for _, want := range []string{"Assignment prompt", "first assignment prompt", "previous guidance", "latest guidance", "Earlier messages omitted"} {
		if !strings.Contains(detail, want) {
			t.Error("live sidebar detail omitted", want)
		}
	}
	for _, hidden := range []string{"private human main-session transcript", "oldest guidance", "rejected stale guidance", "rejected oversized guidance", "rejected child guidance"} {
		if strings.Contains(detail, hidden) {
			t.Error("job prompt inspection exposed unrelated or rejected context", hidden)
		}
	}
	assertChildInspectionOutsideRuntimeLocks(t, r, firstJob)
	releaseFirst()
	if _, err := r.Jobs.Wait(context.Background(), "main", firstJob, nil); err != nil {
		t.Fatal(err)
	}
	finished := inspect(firstJob)
	if finished.Job.Status != "completed" || !reflect.DeepEqual(live.Spec.Fields, finished.Spec.Fields) || len(finished.Output) != 2 || !strings.Contains(finished.Output[0].Text, "child output") {
		t.Fatal("finished job lost its prompt snapshot or default output", finished)
	}
	phase.Store(2)
	followup, _ := json.Marshal(map[string]any{"persistent": false, "prompt": "second assignment prompt", "child_id": first["child_id"], "background": true})
	second := childInvocation(t, r, calls[0], string(followup))
	if second["ok"] != true || second["child_id"] != first["child_id"] || second["job_id"] == firstJob {
		t.Fatal("persistent context was not reused with a distinct job", first, second)
	}
	receive(t, secondStarted)
	secondJob := second["job_id"].(string)
	if result := sendChildSteer(t, r, "main", calls[0], map[string]any{"job_id": secondJob, "message": "second assignment guidance"}); result["ok"] != true {
		t.Fatal(result)
	}
	if result := sendChildSteer(t, r, "main", calls[0], map[string]any{"job_id": firstJob, "message": "rejected finished guidance"}); result["ok"] == true {
		t.Fatal("finished assignment accepted guidance", result)
	}
	if current := childInspectionValues(inspect(secondJob).Spec); len(current) != 3 || current["Assignment prompt"] != "second assignment prompt" || current["Latest parent steering"] != "second assignment guidance" {
		t.Fatal("new assignment inherited old presentation context", current)
	}
	if old := inspect(firstJob); !reflect.DeepEqual(finished.Spec.Fields, old.Spec.Fields) {
		t.Fatal("persistent reuse redirected finished job inspection", old.Spec.Fields)
	}
	releaseSecond()
	if _, err := r.Jobs.Wait(context.Background(), "main", secondJob, nil); err != nil {
		t.Fatal(err)
	}
	if old := inspect(firstJob); !reflect.DeepEqual(finished.Spec.Fields, old.Spec.Fields) {
		t.Fatal("closing child discarded finished job prompt inspection", old.Spec.Fields)
	}
}

func assertChildInspectionOutsideRuntimeLocks(t *testing.T, r *Runtime, id string) {
	t.Helper()
	r.coordinator.childMu.Lock()
	r.routeMu.Lock()
	r.orderMu.Lock()
	done := make(chan error, 1)
	go func() { _, err := r.Jobs.Inspect(context.Background(), "main", id); done <- err }()
	var err error
	timedOut := false
	select {
	case err = <-done:
	case <-time.After(2 * time.Second):
		timedOut = true
	}
	r.orderMu.Unlock()
	r.routeMu.Unlock()
	r.coordinator.childMu.Unlock()
	if timedOut {
		<-done
		t.Fatal("job inspector acquired a runtime admission/routing lock")
	}
	if err != nil {
		t.Fatal(err)
	}
}
