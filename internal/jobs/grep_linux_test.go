package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"ttc/internal/capture"
)

type grepObservedContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *grepObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestGrepScanAdmissionBoundAndCancellation(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("runtime_close=%t", shutdown), func(t *testing.T) {
			m := New(context.Background(), nil)
			defer m.Close()
			if cap(m.grepScans) != 2 {
				t.Fatal("unexpected scan bound", cap(m.grepScans))
			}
			for range 2 {
				if err := m.acquireGrepScan(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observed := &grepObservedContext{Context: ctx, entered: make(chan struct{})}
			result := make(chan error, 1)
			go func() { result <- m.acquireGrepScan(observed) }()
			<-observed.entered // Both slots are occupied when the waiter selects.
			select {
			case err := <-result:
				t.Fatal("third scan admitted", err)
			default:
			}
			if shutdown {
				m.Close()
			} else {
				cancel()
			}
			if err := <-result; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if len(m.grepScans) != 2 {
				t.Fatal("canceled waiter changed admission count")
			}
			for range 2 {
				<-m.grepScans
			}
		})
	}
}

type grepTestWriter func([]byte) (int, error)

func (w grepTestWriter) Write(p []byte) (int, error) { return w(p) }

func TestGrepBatchesManyShortMatchingLines(t *testing.T) {
	raw := strings.Repeat("\n", 1<<20)
	var got strings.Builder
	writes := 0
	writer := grepTestWriter(func(p []byte) (int, error) {
		writes++
		if len(p) > grepChunkBytes {
			t.Fatal("unbounded write", len(p))
		}
		return got.Write(p)
	})
	count, err := scanGrep(context.Background(), raw, regexp.MustCompile("^$"), writer)
	if err != nil || count != int64(len(raw)) || got.String() != raw || writes != len(raw)/grepChunkBytes {
		t.Fatal(count, writes, err)
	}
}

func TestGrepChunkFlushCancellation(t *testing.T) {
	for _, raw := range []string{strings.Repeat("\n", 3*grepChunkBytes), strings.Repeat("x", 3*grepChunkBytes)} {
		ctx, cancel := context.WithCancel(context.Background())
		writes := 0
		writer := grepTestWriter(func(p []byte) (int, error) {
			writes++
			if len(p) != grepChunkBytes {
				t.Fatal("unexpected chunk size", len(p))
			}
			cancel()
			return len(p), nil
		})
		_, err := scanGrep(ctx, raw, regexp.MustCompile(".*"), writer)
		cancel()
		if !errors.Is(err, context.Canceled) || writes != 1 {
			t.Fatal(writes, err)
		}
	}
}

func grepFixture(t *testing.T, m *Manager, owner, raw string) string {
	t.Helper()
	id, err := m.StartTask(owner, TaskOptions{Kind: "test"}, func(_ context.Context, out, _ io.Writer) (TaskResult, error) {
		_, err := io.WriteString(out, raw)
		return TaskResult{}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Wait(context.Background(), owner, id, nil); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestGrepWholeRawSnapshotAndPagination(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	long := "START" + strings.Repeat("x", 150000) + "FINISH\n"
	raw := strings.Repeat("ignored\n", 12000) + long + "\x1b[31mcolored\x1b[0m\nlast"
	id := grepFixture(t, m, "main", raw)
	strip := false
	first, err := m.Read(context.Background(), "main", id, ReadOptions{Grep: "START.*FINISH|\\x1b\\[31m|^last$", Limit: 4096, StripANSI: &strip})
	if err != nil {
		t.Fatal(err)
	}
	gid := first["job_id"].(string)
	origin := first["grep_origin"].(GrepOrigin)
	if gid == id || origin.MatchCount != 3 || origin.EndByte != int64(len(raw)) || origin.StartByte != 0 || origin.Truncated {
		t.Fatal(first)
	}
	count := len(m.Metadata("main"))
	var all strings.Builder
	page := first
	for {
		all.WriteString(page["output"].(string))
		cursor := page["next_cursor"]
		if cursor == nil {
			break
		}
		page, err = m.Read(context.Background(), "main", gid, ReadOptions{Cursor: cursor.(string), Limit: 4096, StripANSI: &strip})
		if err != nil {
			t.Fatal(err)
		}
	}
	if want := long + "\x1b[31mcolored\x1b[0m\nlast"; all.String() != want {
		t.Fatalf("got %d bytes, want %d", all.Len(), len(want))
	}
	if len(m.Metadata("main")) != count {
		t.Fatal("pagination created jobs")
	}
	nested, err := m.Read(context.Background(), "main", gid, ReadOptions{Grep: "colored", Limit: 1024})
	if err != nil || nested["output"] != "colored\n" {
		t.Fatal(nested, err)
	}
	inspected, err := m.Inspect(context.Background(), "main", gid)
	if err != nil || len(inspected.Spec.Fields) != 4 || inspected.Job.Grep.SourceJobID != id {
		t.Fatal(inspected, err)
	}
}

func TestGrepRawMatchingIndependentOfPresentation(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	id := grepFixture(t, m, "main", "\x1b[31mword\x1b[0m\n")
	page, err := m.Read(context.Background(), "main", id, ReadOptions{Grep: "\x1b[31m", Literal: true, Limit: 1024})
	if err != nil || page["output"] != "word\n" {
		t.Fatal(page, err)
	}
	page, err = m.Read(context.Background(), "main", id, ReadOptions{Grep: "^word$", Limit: 1024})
	if err != nil || page["output"] != "" || page["next_cursor"] != nil || page["grep_origin"].(GrepOrigin).MatchCount != 0 {
		t.Fatal(page, err)
	}
}

func TestGrepPublishesCompletedStateOnceWithoutNotification(t *testing.T) {
	notified := false
	m := New(context.Background(), func(Snapshot) { notified = true })
	defer m.Close()
	id := grepFixture(t, m, "main", "hit one\nhit two\n")
	var states []Snapshot
	m.OnState = func(s Snapshot) {
		if s.Kind != "grep" {
			t.Fatalf("unexpected publication: %+v", s)
		}
		// View acquires the manager lock and verifies publication precedes the
		// callback, while the callback itself runs outside that lock.
		view, err := m.View("main", s.ID)
		if err != nil || view.Status != "completed" || s.Status != "completed" || s.Grep == nil || s.Grep.SourceJobID != id {
			t.Fatal(s, view, err)
		}
		states = append(states, s)
		// Callbacks receive independent metadata, not the job's live pointer.
		s.Grep.Pattern = "callback mutation"
	}
	page, err := m.Read(context.Background(), "main", id, ReadOptions{Grep: "hit", Limit: 8})
	if err != nil || len(states) != 1 || notified || page["grep_origin"].(GrepOrigin).Pattern != "hit" {
		t.Fatal(page, states, notified, err)
	}
	gid := page["job_id"].(string)
	if _, err = m.Read(context.Background(), "main", gid, ReadOptions{Cursor: page["next_cursor"].(string), Limit: 8}); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Stop("main", gid); err != nil || len(states) != 1 || notified {
		t.Fatal(states, notified, err)
	}
}

func TestGrepRunningSnapshotImmutableAndSilent(t *testing.T) {
	notifications := make(chan Snapshot, 4)
	m := New(context.Background(), func(s Snapshot) { notifications <- s })
	defer m.Close()
	ready, next := make(chan struct{}), make(chan struct{})
	id, err := m.StartTask("main/child", TaskOptions{Kind: "test"}, func(ctx context.Context, out, _ io.Writer) (TaskResult, error) {
		io.WriteString(out, "hit first\n")
		close(ready)
		select {
		case <-next:
		case <-ctx.Done():
			return TaskResult{}, ctx.Err()
		}
		io.WriteString(out, "hit later\n")
		return TaskResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-ready
	first, err := m.Read(context.Background(), "main/child", id, ReadOptions{Grep: "hit", Limit: 1024})
	if err != nil || first["output"] != "hit first\n" || first["next_cursor"] != nil {
		t.Fatal(first, err)
	}
	gid := first["job_id"].(string)
	close(next)
	if _, err = m.Wait(context.Background(), "main", id, nil); err != nil {
		t.Fatal(err)
	}
	page, err := m.Read(context.Background(), "main", gid, ReadOptions{Limit: 1024})
	if err != nil || page["output"] != "hit first\n" {
		t.Fatal(page, err)
	}
	select {
	case n := <-notifications:
		t.Fatal("spurious notification", n)
	default:
	}
	if _, err = m.Read(context.Background(), "main/sibling", gid, ReadOptions{Limit: 1024}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err = m.Stop("main/child", gid); err != nil {
		t.Fatal(err)
	}
}

func TestGrepValidationRetentionAndCleanup(t *testing.T) {
	m := New(context.Background(), nil)
	m.pool = capture.NewPool(2048)
	id := grepFixture(t, m, "main/child", strings.Repeat("discarded\n", 300)+"tail hit\n")
	page, err := m.Read(context.Background(), "main/child", id, ReadOptions{Grep: "hit", Limit: 1024})
	if err != nil || page["output"] != "tail hit\n" || page["truncated"] != true {
		t.Fatal(page, err)
	}
	origin := page["grep_origin"].(GrepOrigin)
	if !origin.Truncated || origin.StartByte <= 0 {
		t.Fatal(origin)
	}
	before := len(m.Metadata("main"))
	// Virtual streams use ordinary shared retention, but retain immutable origin
	// metadata even if subsequent jobs evict all matching bytes.
	for range 4 {
		b := m.pool.NewBuffer(1024)
		_, _ = b.Write([]byte(strings.Repeat("z", 1024)))
	}
	lost, err := m.Read(context.Background(), "main", page["job_id"].(string), ReadOptions{Limit: 1024})
	if err != nil || lost["truncated"] != true || lost["output"] != "" || lost["grep_origin"].(GrepOrigin) != origin {
		t.Fatal(lost, err)
	}
	for _, o := range []ReadOptions{{Grep: "hit", Cursor: "0", Limit: 10}, {Grep: "[", Limit: 10}, {Grep: "hit", Stream: "combined", Limit: 10}, {Grep: "hit", Limit: 0}} {
		if _, err = m.Read(context.Background(), "main", id, o); err == nil {
			t.Fatal("accepted", o)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = m.Read(ctx, "main", id, ReadOptions{Grep: "hit", Limit: 10}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(m.Metadata("main")) != before {
		t.Fatal("failed grep created jobs")
	}
	m.StopOwned("main/child")
	if _, err = m.Read(context.Background(), "main/child", id, ReadOptions{Grep: "hit", Limit: 10}); err == nil {
		t.Fatal("sealed owner created grep")
	}
	gid := page["job_id"].(string)
	m.Close()
	if used, _ := m.pool.Stats(); used != 0 {
		t.Fatal("capture leaked", used)
	}
	if _, err = m.Read(context.Background(), "main", gid, ReadOptions{Limit: 10}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestGrepScanCancellationJoinsOwner(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	id := grepFixture(t, m, "main/child", strings.Repeat("0123456789abcdef\n", 1000000))
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	before := len(m.Metadata("main"))
	if _, err := m.Read(ctx, "main/child", id, ReadOptions{Grep: ".*", Limit: 16}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if len(m.Metadata("main")) != before {
		t.Fatal("cancelled scan published a job")
	}
	m.StopOwned("main/child")
}
