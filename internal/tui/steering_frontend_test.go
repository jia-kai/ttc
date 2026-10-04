package tui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"ttc/internal/provider"
)

func TestAltEnterSteersWithoutSubmittingQueuedTurn(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "modified_enter", true: "legacy_tty"}[legacy], func(t *testing.T) { testAltEnterSteering(t, legacy) })
	}
}

func testAltEnterSteering(t *testing.T, legacy bool) {
	p := &questionTestProvider{Script: provider.Script{Responses: []provider.ScriptResponse{{Text: "First boundary settles"}, {Text: "Steering accepted"}}}, ready: make(chan struct{}), release: make(chan struct{})}
	u := newQuestionTestUI(t, p)
	u.typeText("start")
	u.key(tcell.KeyEnter)
	select {
	case <-p.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	u.typeText("change direction")
	if legacy {
		u.screen.PostEventWait(tcell.NewEventKey(tcell.KeyRune, 'm', tcell.ModAlt|tcell.ModCtrl))
	} else {
		u.screen.PostEventWait(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModAlt))
	}
	u.wait(t, "Steer · change direction")
	close(p.release)
	u.wait(t, "Steering accepted")
	u.wait(t, "Turn complete")
	var codingTurns int
	if err := u.runtime.Store.DB.QueryRow("SELECT count(DISTINCT turn_id) FROM model_requests WHERE purpose='coding'").Scan(&codingTurns); err != nil || codingTurns != 1 {
		t.Fatal("steer became a separate coding turn", codingTurns, err)
	}
}
