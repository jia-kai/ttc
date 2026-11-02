package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/historytest"
	"ttc/internal/workspace"
)

func TestPatchAncestorTargetsRejectBeforeMutation(t *testing.T) {
	for _, sections := range []string{
		"*** Add File: parent\n+one\n*** Add File: parent/child\n+two\n",
		"*** Add File: parent/child\n+two\n*** Add File: parent\n+one\n",
		"*** Update File: source\n*** Move to: source/child\n",
	} {
		t.Run(strings.Split(sections, "\n")[0], func(t *testing.T) {
			r, w, execution, request := toolFixture(t)
			if err := os.WriteFile(filepath.Join(w.Root, "source"), []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			args, err := json.Marshal(map[string]string{"patch_text": "*** Begin Patch\n" + sections + "*** End Patch"})
			if err != nil {
				t.Fatal(err)
			}
			record := invoke(t, r, w, execution, request, "patch", string(args))
			if !strings.Contains(string(record.Result), `"ok":false`) || !strings.Contains(string(record.Result), "conflicting mutation targets") {
				t.Fatalf("missing target conflict: %s", record.Result)
			}
			saved, err := w.Store.Session(execution.SessionID)
			if err != nil || saved.FileTip != 0 {
				t.Fatalf("preflight advanced file history: %+v, %v", saved, err)
			}
			if _, err := os.Stat(filepath.Join(w.Root, "parent")); !os.IsNotExist(err) {
				t.Fatal("preflight mutated parent", err)
			}
			data, err := os.ReadFile(filepath.Join(w.Root, "source"))
			if err != nil || string(data) != "original" {
				t.Fatalf("preflight moved source: %q, %v", data, err)
			}
		})
	}
}

func TestMutationHistoryFailureReportsAppliedEffects(t *testing.T) {
	for _, phase := range []string{"before-insert", "after-id"} {
		for _, name := range []string{"write", "edit", "patch", "move"} {
			t.Run(phase+"/"+name, func(t *testing.T) {
				r, w, execution, request := toolFixture(t)
				path := filepath.Join(w.Root, "saved")
				if err := os.WriteFile(path, []byte("original\n"), 0600); err != nil {
					t.Fatal(err)
				}
				trigger := `CREATE TRIGGER fail_change BEFORE INSERT ON file_changes BEGIN SELECT RAISE(ABORT, 'injected history failure'); END`
				if phase == "after-id" {
					trigger = `CREATE TRIGGER fail_change BEFORE UPDATE OF file_tip_id ON sessions BEGIN SELECT RAISE(ABORT, 'injected history failure'); END`
				}
				if _, err := historytest.DB(t, w.Store).Exec(trigger); err != nil {
					t.Fatal(err)
				}
				toolName := name
				args := map[string]any{"path": "saved", "content": "written\n"}
				applied := []string{path}
				switch name {
				case "edit":
					args = map[string]any{"path": "saved", "old_text": "original", "new_text": "written"}
				case "patch":
					args = map[string]any{"patch_text": "*** Begin Patch\n*** Update File: saved\n@@\n-original\n+written\n*** End Patch"}
				case "move":
					toolName = "patch"
					args = map[string]any{"patch_text": "*** Begin Patch\n*** Update File: saved\n*** Move to: moved\n@@\n-original\n+written\n*** End Patch"}
					applied = append(applied, filepath.Join(w.Root, "moved"))
				}
				encoded, err := json.Marshal(args)
				if err != nil {
					t.Fatal(err)
				}
				record := invoke(t, r, w, execution, request, toolName, string(encoded))
				var envelope struct {
					OK    bool `json:"ok"`
					Error struct {
						Code    string `json:"code"`
						Message string `json:"message"`
						Details struct {
							Applied         []string `json:"applied"`
							Remaining       []string `json:"remaining"`
							HistoryRecorded *bool    `json:"history_recorded"`
							Undoable        *bool    `json:"undoable"`
							RetrySafe       *bool    `json:"retry_safe"`
							ChangeID        *int64   `json:"change_id"`
						} `json:"details"`
					} `json:"error"`
				}
				if err := json.Unmarshal(record.Result, &envelope); err != nil {
					t.Fatal(err)
				}
				details := envelope.Error.Details
				code := "execution_failed"
				if toolName == "patch" {
					code = "partial_patch"
				}
				if envelope.OK || envelope.Error.Code != code || !reflect.DeepEqual(details.Applied, applied) || details.Remaining == nil || len(details.Remaining) != 0 || details.ChangeID != nil {
					t.Fatalf("incorrect failure envelope: %s", record.Result)
				}
				for key, value := range map[string]*bool{"history_recorded": details.HistoryRecorded, "undoable": details.Undoable, "retry_safe": details.RetrySafe} {
					if value == nil || *value {
						t.Fatalf("%s must explicitly be false: %s", key, record.Result)
					}
				}
				if !strings.Contains(envelope.Error.Message, "injected history failure") || !strings.Contains(envelope.Error.Message, "cannot be undone") {
					t.Fatalf("missing recovery guidance: %s", record.Result)
				}
				if name == "move" {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatal("move did not remove source", err)
					}
					path = filepath.Join(w.Root, "moved")
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "written\n" {
					t.Fatalf("reported write not on disk: %q, %v", data, err)
				}
				saved, err := w.Store.Session(execution.SessionID)
				if err != nil || saved.FileTip != 0 {
					t.Fatalf("failed history committed: %+v, %v", saved, err)
				}
				var count int
				if err := historytest.DB(t, w.Store).QueryRow(`SELECT count(*) FROM file_changes`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("rolled-back file change survived: %d, %v", count, err)
				}
			})
		}
	}
}

func TestMutationFailureProjectionUsesAppliedFlags(t *testing.T) {
	for _, patch := range []bool{false, true} {
		for _, recorded := range []bool{false, true} {
			res := workspace.Result{HistoryRecorded: recorded, Reversible: true, Changes: []workspace.PathChange{
				{Path: "/applied", Applied: true}, {Path: "/remaining"},
			}}
			if recorded {
				res.ChangeID = 42
			}
			// The workspace fault-injection tests exercise actual post-rename,
			// cancellation and combined SQL failures; here test their shared projection.
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // Avoid running diff; cancellation must not hide applied effects.
			_, err := mutationResult(ctx, nil, res, context.Canceled, patch)
			var failure *Error
			if !errors.As(err, &failure) {
				t.Fatal("missing structured failure", err)
			}
			code := "cancelled"
			if patch {
				code = "partial_patch"
			}
			details := failure.Details.(map[string]any)
			if failure.Code != code || !reflect.DeepEqual(details["applied"], []string{"/applied"}) || !reflect.DeepEqual(details["remaining"], []string{"/remaining"}) || details["history_recorded"] != recorded || details["undoable"] != recorded {
				t.Fatalf("wrong applied projection: %+v", failure)
			}
			id, exists := details["change_id"]
			if exists != recorded || recorded && id != int64(42) {
				t.Fatalf("uncommitted ID projected: %+v", failure)
			}
		}
	}
}
