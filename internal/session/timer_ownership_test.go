package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ttc/internal/tool"
)

func TestTimerSelectorsMustAgreeAndRespectOwnership(t *testing.T) {
	r, _ := timerInspectionFixture(t)
	first := invokeTimerTool(t, r, "main/child_one", "wakeup_schedule", map[string]any{"name": "one", "message": "later", "delay_seconds": 3600})
	second := invokeTimerTool(t, r, "main/child_two", "wakeup_schedule", map[string]any{"name": "two", "message": "later", "delay_seconds": 3600})
	invoke := func(actor, name string, args map[string]any) bool {
		input, _ := json.Marshal(args)
		record := r.Tools.Invoke(context.Background(), tool.Execution{Actor: actor}, name, input)
		return strings.Contains(string(record.Result), `"ok":true`)
	}
	for _, name := range []string{"wakeup_cancel", "wakeup_update"} {
		owned := map[string]any{"wakeup_id": first["wakeup_id"]}
		conflict := map[string]any{"wakeup_id": first["wakeup_id"], "name": "two"}
		if name == "wakeup_update" {
			owned["message"] = "changed"
			conflict["message"] = "changed"
		}
		if invoke("main/child_two", name, owned) {
			t.Fatal("cross-owner selector accepted", name)
		}
		if invoke("main", name, conflict) {
			t.Fatal("conflicting selectors accepted", name, second)
		}
	}
	if !invoke("main/child_one", "wakeup_update", map[string]any{"wakeup_id": first["wakeup_id"], "name": "one", "message": "changed"}) {
		t.Fatal("matching redundant update rejected")
	}
	if !invoke("main/child_one", "wakeup_cancel", map[string]any{"wakeup_id": first["wakeup_id"], "name": "one"}) {
		t.Fatal("matching redundant cancellation rejected")
	}
}
