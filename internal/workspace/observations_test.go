package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFileKnowledgeRewriteRule(t *testing.T) {
	old := contentFingerprint{exists: true, hash: hash([]byte("old"))}
	newContent := contentFingerprint{exists: true, hash: hash([]byte("new"))}
	w := &Manager{}
	k := w.fileKnowledge("file")
	k.reconcile(old)
	if !k.allowWrite("parent", old) {
		t.Fatal("unmodified existing file requires an unnecessary read")
	}
	k.modified("parent", old)
	if !k.allowWrite("parent", old) || k.allowWrite("child", old) {
		t.Fatal("self-only modification rule is incorrect")
	}
	k.observed["child"] = old
	k.modified("parent", newContent)
	if k.allowWrite("child", newContent) {
		t.Fatal("stale observation accepted")
	}
	k.modified("parent", old)
	if !k.allowWrite("child", old) {
		t.Fatal("equal content hash must permit a write even after intervening changes")
	}
	k.modified("parent", newContent)
	k.reconcile(old)
	if k.allowWrite("child", old) {
		t.Fatal("untracked divergence must require a fresh read even if an old observation matches")
	}
	k.reconcile(newContent)
	if k.allowWrite("parent", newContent) {
		t.Fatal("external change incorrectly attributed to self")
	}
	k.observed["parent"] = newContent
	if !k.allowWrite("parent", newContent) {
		t.Fatal("fresh observation did not resolve external change")
	}
}

func TestConcurrentBlindFullWritesHaveOneWinner(t *testing.T) {
	w, saved, turn, request := fixture(t)
	calls := []string{intent(t, w, saved, turn, request), intent(t, w, saved, turn, request)}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i, actor := range []string{"parent", "child"} {
		go func(actor, call string) {
			<-start
			_, err := w.Apply(context.Background(), saved.ID, call, []Mutation{{Actor: actor, FullWrite: true, Path: "file", Data: []byte(actor)}})
			results <- err
		}(actor, calls[i])
	}
	close(start)
	var successes, stale int
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if strings.Contains(err.Error(), "Full write rejected") {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("validation and mutation were not serialized: success=%d stale=%d", successes, stale)
	}
}

func TestObserveReadDoesNotBlockUnrelatedPaths(t *testing.T) {
	w, saved, turn, request := fixture(t)
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(w.Root, name), []byte("initial"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		readDone <- w.ObserveRead(context.Background(), "reader", "a", func() (string, error) {
			close(entered)
			<-release
			return hash([]byte("initial")), nil
		})
	}()
	defer func() {
		close(release)
		if err := <-readDone; err != nil {
			t.Error(err)
		}
	}()
	<-entered
	call := intent(t, w, saved, turn, request)
	otherDone := make(chan error, 1)
	go func() {
		if err := w.ObserveRead(context.Background(), "reader", "b", func() (string, error) {
			return hash([]byte("initial")), nil
		}); err != nil {
			otherDone <- err
			return
		}
		_, err := w.Apply(context.Background(), saved.ID, call, []Mutation{{Actor: "writer", FullWrite: true, Path: "b", Data: []byte("changed")}})
		otherDone <- err
	}()
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked read on a prevented unrelated read/write on b")
	}
}

func TestObserveReadOverlappingWritePreservesNewerTrackedState(t *testing.T) {
	for _, sameActor := range []bool{false, true} {
		t.Run(fmt.Sprint("same-actor=", sameActor), func(t *testing.T) {
			w, saved, turn, request := fixture(t)
			path := filepath.Join(w.Root, "file")
			if err := os.WriteFile(path, []byte("initial"), 0600); err != nil {
				t.Fatal(err)
			}
			// Make the matching-observation branch necessary, even for self writes.
			_, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), []Mutation{{Actor: "other", Path: "file", Data: []byte("initial")}})
			if err != nil {
				t.Fatal(err)
			}
			if err := w.ObserveRead(context.Background(), "writer", path, func() (string, error) { return hash([]byte("initial")), nil }); err != nil {
				t.Fatal(err)
			}
			actor := "reader"
			readPath := path
			if sameActor {
				actor = "writer"
			} else {
				readPath = filepath.Join(w.Root, "alias")
				if err := os.Symlink(path, readPath); err != nil {
					t.Fatal(err)
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			readDone := make(chan error, 1)
			go func() {
				readDone <- w.ObserveRead(context.Background(), actor, readPath, func() (string, error) {
					f, err := os.Open(readPath)
					if err != nil {
						close(entered)
						return "", err
					}
					defer f.Close()
					close(entered)
					<-release
					data, err := io.ReadAll(f)
					return hash(data), err
				})
			}()
			<-entered
			call := intent(t, w, saved, turn, request)
			writeDone := make(chan error, 1)
			go func() {
				_, err := w.Apply(context.Background(), saved.ID, call, []Mutation{{Actor: "writer", FullWrite: true, Path: "file", Data: []byte("changed")}})
				writeDone <- err
			}()
			var writeErr error
			select {
			case writeErr = <-writeDone:
			case <-time.After(5 * time.Second):
				close(release)
				<-readDone
				<-writeDone
				t.Fatal("read callback blocked same-path mutation")
			}
			close(release)
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			if err := <-readDone; err != nil {
				t.Fatal(err)
			}
			k := w.files[path]
			if k.current != (contentFingerprint{exists: true, hash: hash([]byte("changed"))}) || k.untracked {
				t.Fatalf("late read replaced newer state or invented external divergence: %+v", k)
			}
			if k.observed[actor].hash != hash([]byte("initial")) {
				t.Fatal("read was credited with concurrent write's bytes instead of its snapshot")
			}
			if !sameActor && k.observed["writer"].hash != hash([]byte("changed")) {
				t.Fatal("late read cleared peer's successful write observation")
			}
			_, err = w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), []Mutation{{Actor: actor, FullWrite: true, Path: "file", Data: []byte("stale")}})
			if err == nil || !strings.Contains(err.Error(), "Full write rejected") {
				t.Fatal("stale concurrent snapshot permitted overwrite", err)
			}
		})
	}
}

func TestCanceledObserveReadDoesNotRecordSuccessfulCallback(t *testing.T) {
	w, _, _, _ := fixture(t)
	path := filepath.Join(w.Root, "file")
	if err := os.WriteFile(path, []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- w.ObserveRead(ctx, "reader", path, func() (string, error) {
			close(entered)
			<-ctx.Done()
			return hash([]byte("initial")), nil
		})
	}()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(w.files) != 0 {
		t.Fatal("canceled callback recorded an observation")
	}
}

func TestConcurrentReadsDoNotReplaceNewerKnowledge(t *testing.T) {
	for _, initiallyKnown := range []bool{false, true} {
		t.Run(fmt.Sprint("initially-known=", initiallyKnown), func(t *testing.T) {
			w, _, _, _ := fixture(t)
			path := filepath.Join(w.Root, "file")
			if err := os.WriteFile(path, []byte("initial"), 0600); err != nil {
				t.Fatal(err)
			}
			if initiallyKnown {
				if err := w.ObserveRead(context.Background(), "baseline", path, func() (string, error) { return hash([]byte("initial")), nil }); err != nil {
					t.Fatal(err)
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- w.ObserveRead(context.Background(), "older", path, func() (string, error) {
					close(entered)
					<-release
					return hash([]byte("initial")), nil
				})
			}()
			<-entered
			newerDone := make(chan error, 1)
			go func() {
				newerDone <- w.ObserveRead(context.Background(), "newer", path, func() (string, error) {
					return hash([]byte("newer")), nil
				})
			}()
			select {
			case err := <-newerDone:
				if err != nil {
					close(release)
					<-done
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				close(release)
				<-done
				<-newerDone
				t.Fatal("slow read blocked newer read")
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			k := w.files[path]
			if k.current.hash != hash([]byte("newer")) || k.untracked != initiallyKnown || len(k.observed) != 2 {
				t.Fatalf("late read replaced newer knowledge or cleared peer observation: %+v", k)
			}
		})
	}
}

func TestFailedObserveReadDoesNotCreditHash(t *testing.T) {
	w, _, _, _ := fixture(t)
	path := filepath.Join(w.Root, "file")
	if err := os.WriteFile(path, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected read failure")
	if err := w.ObserveRead(context.Background(), "reader", path, func() (string, error) {
		return hash([]byte("content")), failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if len(w.files) != 0 {
		t.Fatal("failed read recorded state")
	}
}

func TestSuccessfulWriteObservationIsQueuedBeforeNextActor(t *testing.T) {
	w, saved, turn, request := fixture(t)
	var wg sync.WaitGroup
	for _, actor := range []string{"parent", "child"} {
		_, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), []Mutation{{Actor: actor, Path: actor, Data: []byte("initial")}})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Another modifier makes the observation necessary on subsequent writes.
	_, err := w.Apply(context.Background(), saved.ID, intent(t, w, saved, turn, request), []Mutation{{Actor: "other", Path: "parent", Data: []byte("initial")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.ObserveRead(context.Background(), "parent", "parent", func() (string, error) { return hash([]byte("initial")), nil }); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		call := intent(t, w, saved, turn, request)
		wg.Go(func() {
			if _, err := w.Apply(context.Background(), saved.ID, call, []Mutation{{Actor: "parent", FullWrite: true, Path: "parent", Data: []byte("next")}}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}
