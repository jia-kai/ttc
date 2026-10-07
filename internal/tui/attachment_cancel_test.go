package tui

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/blobcache"
	"ttc/internal/filelock"
	"ttc/internal/llm"
	"ttc/internal/session"
)

func TestSessionSwitchCancelsBlockedAttachment(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	u := newQuestionTestUIWithSetup(t, &llm.Script{}, nil, func(r *session.Runtime) {
		selection := r.CurrentSelection()
		selection.Model.ID = "scripted-images"
		selection.Model.Images = true
		if err := r.RequestModel(selection); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ApplyModel(""); err != nil {
			t.Fatal(err)
		}
	})
	cache, err := blobcache.Default()
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(cache.Root, ".lock")
	lock, err := filelock.Acquire(context.Background(), lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	// A valid image reaches the shared cache lock. Observe its open lock fd
	// rather than sleeping, so the session switch always interrupts a waiter.
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(u.runtime.Workspace.Root, "blocked.png")
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	waitLockFDs := func(want int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			entries, err := os.ReadDir("/proc/self/fd")
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, entry := range entries {
				target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
				if err == nil && target == lockPath {
					count++
				}
			}
			if count == want {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("attachment lock fd count did not reach %d", want)
	}
	u.typeText("/attach " + path)
	u.key(tcell.KeyEnter)
	waitLockFDs(2)

	if _, err := u.runtime.Command("/new"); err != nil {
		t.Fatal(err)
	}
	// Keep our exclusive lock held: cancellation, not lock release, must
	// terminate the old worker and allow attachments in the new session.
	waitLockFDs(1)
	textPath := filepath.Join(u.runtime.Workspace.Root, "replacement.txt")
	if err := os.WriteFile(textPath, []byte("new session evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	u.typeText("/attach " + textPath)
	u.key(tcell.KeyEnter)
	frame := u.wait(t, "1 attachments")
	if strings.Contains(frame, "Attachment failed:") || strings.Contains(frame, "Attached · "+path) {
		t.Fatal("stale attachment result reached the new session", frame)
	}
}
