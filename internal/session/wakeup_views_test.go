package session

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWakeupListViewsOptIntoBodyAndStartup(t *testing.T) {
	timer := wakeup{ID: "wake_test", Name: "check", Message: "private reminder body", Status: "scheduled", NextAt: "2026-10-15T12:00:00Z", Repeat: 60, Fired: 2, startupText: "original startup notes"}
	for _, details := range []bool{false, true} {
		data, err := json.Marshal(timer.listView(details))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), timer.Message) != details || strings.Contains(string(data), timer.startupText) != details {
			t.Fatal(string(data))
		}
		for _, field := range []string{`"wakeup_id"`, `"next_at"`, `"fired_count"`, `"repeat_seconds"`} {
			if !strings.Contains(string(data), field) {
				t.Fatal(string(data))
			}
		}
	}
}
