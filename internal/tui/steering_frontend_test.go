package tui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/llm"
)

func TestEnterSteersAndAltEnterQueues(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tcell.Key
		rune rune
		mods tcell.ModMask
	}{
		{"modified_enter", tcell.KeyEnter, 0, tcell.ModAlt},
		{"legacy_cr", tcell.KeyRune, 'm', tcell.ModAlt | tcell.ModCtrl},
		{"legacy_lf", tcell.KeyRune, 'j', tcell.ModAlt | tcell.ModCtrl},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &questionTestProvider{Script: llm.Script{Responses: []llm.ScriptResponse{
				{Prefix: "user: start", Text: "First boundary settles"},
				{Prefix: "user: change direction", Text: "Steering accepted"},
				{Prefix: "user: next turn", Text: "Queued turn accepted"},
			}}, ready: make(chan struct{}), release: make(chan struct{})}
			u := newQuestionTestUI(t, p)
			// Alt+Enter still sends an ordinary turn when idle.
			u.typeText("start")
			u.screen.PostEventWait(tcell.NewEventKey(tc.key, tc.rune, tc.mods))
			select {
			case <-p.ready:
			case <-time.After(3 * time.Second):
				t.Fatal("request did not start")
			}
			u.typeText("next turn")
			u.screen.PostEventWait(tcell.NewEventKey(tc.key, tc.rune, tc.mods))
			u.wait(t, "Queued · next turn")
			u.typeText("change direction")
			u.key(tcell.KeyEnter)
			u.wait(t, "Steer · change direction")
			if count, _ := u.runtime.SteeringPreview(0); count != 1 {
				t.Fatal("Enter did not steer exclusively", count)
			}
			var pendingEntries int
			if err := u.runtime.Store.DB.QueryRow(`SELECT count(*) FROM entries WHERE role='user' AND json_extract(content_json,'$.content') IN ('next turn','change direction')`).Scan(&pendingEntries); err != nil || pendingEntries != 0 {
				t.Fatal("pending inputs entered history before admission", pendingEntries, err)
			}
			close(p.release)
			u.wait(t, "Queued turn accepted")
			u.wait(t, "Turn complete")
			var requests, codingTurns int
			if err := u.runtime.Store.DB.QueryRow("SELECT count(*), count(DISTINCT turn_id) FROM model_requests WHERE purpose='coding'").Scan(&requests, &codingTurns); err != nil || requests != 3 || codingTurns != 2 {
				t.Fatal("steer or queue used the wrong coding turn", requests, codingTurns, err)
			}
			messages, err := u.runtime.Store.Messages(u.runtime.Current())
			if err != nil {
				t.Fatal(err)
			}
			var humans []llm.Message
			for _, m := range messages {
				if m.Role == "user" && !m.Runtime {
					humans = append(humans, m)
				}
			}
			if len(humans) != 3 {
				t.Fatal("inputs lost or duplicated", humans)
			}
			for i, want := range []struct{ text, source string }{{"start", "normal"}, {"change direction", "steer"}, {"next turn", "queue"}} {
				if humans[i].Content != want.text || humans[i].InputSource != want.source {
					t.Fatal("input order or provenance changed", humans)
				}
			}
		})
	}
}
