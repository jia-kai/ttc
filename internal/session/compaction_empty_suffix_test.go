package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/history"
	"ttc/internal/historytest"
	"ttc/internal/llm"
)

func TestMainCompactionEmptySuffixWithForeignEntryGap(t *testing.T) {
	for _, oversizedTail := range []bool{false, true} {
		t.Run(fmt.Sprintf("oversized_tail=%t", oversizedTail), func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			compactionBudget(t, r)
			seedCompactionHistory(t, r, strings.Repeat("Completed research evidence. ", 250))
			before := r.Current()
			selection := r.CurrentSelection()
			original, err := r.Store.Messages(before)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := prepareCompaction(original, selection)
			if err != nil || prepared.retention.Start != len(original) {
				t.Fatal("fixture must have no frozen retained suffix", prepared.retention, err)
			}

			foreign := history.NewID("session")
			turn, _, err := r.Store.StartSession(foreign, r.Workspace.Root, selection, llm.Message{Type: llm.TypeUserInput, Content: "Independent conversation."})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Store.FinishTurn(turn, "completed"); err != nil {
				t.Fatal(err)
			}
			db := historytest.DB(t, r.Store)
			// Request admission happens after the frozen cut, before any summary
			// audit entries. Consume its next global ID in another session and
			// extend only that session's branch, deterministically and test-only.
			_, err = db.Exec(fmt.Sprintf(`CREATE TRIGGER foreign_entry_at_compaction
				AFTER INSERT ON model_requests
				WHEN NEW.session_id='%s' AND NEW.actor_id='main' AND NEW.purpose='compaction'
				BEGIN
					INSERT INTO entries(session_id,parent_id,actor_id,kind,role,model_visible,content_json,file_tip_id,created_ms)
					SELECT id,active_entry_id,'main','message','assistant',1,
						json_object('type','assistant','content','FOREIGN_GAP_EVIDENCE'),file_tip_id,NEW.created_ms
					FROM sessions WHERE id='%s';
					UPDATE sessions SET active_entry_id=last_insert_rowid() WHERE id='%s';
				END`, before, foreign, foreign))
			if err != nil {
				t.Fatal(err)
			}

			const summary = "Completed evidence summarized."
			tail := llm.Message{Type: llm.TypeRuntimeNotification, Content: "OWN_POSTCUT_EVIDENCE"}
			if oversizedTail {
				tail.Content = strings.Repeat(tail.Content, 10000)
			}
			var sourceTip, foreignID, firstAudit, tailID int64
			calls := 0
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				calls++
				if !req.NoTools || !compactionMessagesContain(req.Messages, "Completed research evidence.") || compactionMessagesContain(req.Messages, "FOREIGN_GAP_EVIDENCE") || compactionMessagesContain(req.Messages, "OWN_POSTCUT_EVIDENCE") {
					t.Fatal("summary must contain only the frozen main projection")
				}
				if err := db.QueryRow(`SELECT max(id) FROM entries WHERE session_id=? AND json_extract(content_json,'$.type')='compaction_started'`, before).Scan(&sourceTip); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow(`SELECT id FROM entries WHERE session_id=? AND json_extract(content_json,'$.content')='FOREIGN_GAP_EVIDENCE'`, foreign).Scan(&foreignID); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow(`SELECT min(id) FROM entries WHERE session_id=? AND id>?`, before, sourceTip).Scan(&firstAudit); err != nil {
					t.Fatal(err)
				}
				if foreignID != sourceTip+1 || firstAudit != foreignID+1 {
					t.Fatalf("gap was not reproduced: source=%d foreign=%d first audit=%d", sourceTip, foreignID, firstAudit)
				}
				old, err := r.Store.Session(before)
				if err != nil || old.ReadOnly || old.CompactionError != "" || r.checkContext(false) != nil {
					t.Fatal("predecessor became unusable or read-only before commit", old, err)
				}
				tailID, err = r.Store.Append(before, "", "main", "message", tail.Role(), true, tail)
				if err != nil {
					t.Fatal(err)
				}
				return emit(llm.StreamEvent{Kind: "text", Text: summary})
			}}

			_, compactErr := r.compactContext(context.Background(), "", selection, nil, false)
			old, err := r.Store.Session(before)
			if err != nil || calls != 1 || old.CompactionError != "" || r.checkContext(false) != nil {
				t.Fatalf("compaction left unusable context: calls=%d predecessor=%+v metadata_error=%v compaction_error=%v", calls, old, err, compactErr)
			}
			if oversizedTail {
				var capacity *compactionCapacityError
				if !errors.As(compactErr, &capacity) || !recoverableCompaction(compactErr) || old.ReadOnly || r.Current() != before {
					t.Fatal("oversized own postcut tail must refuse handoff without freezing predecessor", old, compactErr)
				}
				var commits int
				if err := db.QueryRow(`SELECT count(*) FROM compactions WHERE predecessor_id=?`, before).Scan(&commits); err != nil || commits != 0 {
					t.Fatal("rejected handoff published a continuation", commits, err)
				}
				return
			}
			if compactErr != nil || !old.ReadOnly || r.Current() == before {
				t.Fatal("fitting handoff did not commit", old, compactErr)
			}
			loaded, err := r.Store.Load(before)
			if err != nil || !loaded.ReadOnly || loaded.CompactionError != "" {
				t.Fatal("successful predecessor must remain inspectable and usable read-only", loaded, err)
			}
			var committedSource int64
			if err := db.QueryRow(`SELECT source_tip_id FROM compactions WHERE continuation_id=? AND predecessor_id=?`, r.Current(), before).Scan(&committedSource); err != nil || committedSource != sourceTip {
				t.Fatal("global ID gap moved the frozen ancestry cut", committedSource, sourceTip, err)
			}
			got, err := r.Store.Messages(r.Current())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1+len(prepared.inputs)+1 || !reflect.DeepEqual(got[0], llm.NewCompactionContext(summary)) || got[0].Role() != "developer" || !reflect.DeepEqual(got[len(got)-1], tail) {
				t.Fatal("handoff must contain canonical developer context, retained human inputs, and own tail once", got)
			}
			for i, input := range prepared.inputs {
				if got[i+1].Type != llm.TypeUserInput || got[i+1].Content != input.Content {
					t.Fatal("handoff changed retained human input", got[i+1], input)
				}
			}
			cm, _, err := r.runtimeContext(context.Background(), "main", selection, contextCursor{})
			if err != nil || !compactionFits(selection, systemTemplate, r.Tools.Definitions(), got, nil, cm) {
				t.Fatal("persisted canonical handoff and postcut tail do not fit", err)
			}
			entries, err := r.Store.Branch(r.Current(), 0)
			if err != nil {
				t.Fatal(err)
			}
			copies := 0
			for _, entry := range entries {
				if entry.Source == foreignID || strings.Contains(string(entry.Content), "FOREIGN_GAP_EVIDENCE") {
					t.Fatal("handoff imported foreign history", entry)
				}
				if entry.Source == tailID {
					copies++
				}
			}
			if copies != 1 {
				t.Fatal("own postcut tail must be copied exactly once", copies)
			}
		})
	}
}
