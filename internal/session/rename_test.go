package session

import (
	"strings"
	"testing"
	"time"

	"scicode/internal/provider"
)

func TestManualRenameWinsPendingAutomaticNaming(t *testing.T) {
	r, events := runtimeFixture(t, nil)
	p := &gatedNamingProvider{naming: make(chan provider.Request, 1), release: make(chan struct{})}
	r.Provider, r.AutoName = p, true
	message := provider.Message{Role: "user", Content: "Inspect this project"}
	if err := r.Run(&message); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.naming:
	case <-time.After(time.Second):
		t.Fatal("automatic naming did not start")
	}
	if result, err := r.Command("/rename My research session"); err != nil || result != "" {
		t.Fatal(result, err)
	}
	close(p.release)
	<-r.namingDone
	session, err := r.Store.Session(r.Current())
	if err != nil || session.Name != "My research session" {
		t.Fatal(session, err)
	}
	var source string
	if err := r.Store.DB.QueryRow("SELECT name_source FROM sessions WHERE id=?", session.ID).Scan(&source); err != nil || source != "manual" {
		t.Fatal(source, err)
	}
	found := false
	for len(events) > 0 {
		event := <-events
		if event.Kind == "session_name" {
			if event.Text != session.Name || event.SessionID != session.ID {
				t.Fatal("stale automatic name reached UI", event)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("manual name did not reach UI")
	}
	list, err := r.Store.Sessions(r.Workspace.Root)
	if err != nil || len(list) != 1 || list[0].Name != session.Name {
		t.Fatal(list, err)
	}
}

func TestRenameValidationDoesNotPersistBlankSession(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	if _, err := r.Command("/rename An empty session"); err == nil {
		t.Fatal("renamed empty session")
	}
	var count int
	if err := r.Store.DB.QueryRow("SELECT count(*) FROM sessions").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	seedRuntime(t, r, "Inspect the project")
	for _, title := range []string{"", strings.Repeat("界", 61), "invalid\nname", "invalid\x1bname"} {
		if _, err := r.Command("/rename " + title); err == nil {
			t.Fatal("accepted invalid title", title)
		}
	}
	if _, err := r.Command("/rename " + strings.Repeat("界", 60)); err != nil {
		t.Fatal("rejected valid Unicode title", err)
	}
}
