package history

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"ttc/internal/llm"
)

func TestContinuationPendingRecoveryWarningIsAtomicAndDeduplicated(t *testing.T) {
	for _, mode := range []string{"summarized", "retained", "rollback", "missing"} {
		t.Run(mode, func(t *testing.T) {
			s, session, turn, request := historyFixture(t)
			warning := llm.Message{Role: "developer", Runtime: true, RequestID: request, Content: "Mandatory recovery warning"}
			warningID, err := s.Append(session.ID, turn, "main", "message", "developer", true, warning)
			if err != nil {
				t.Fatal(err)
			}
			archive, err := s.ArchiveTranscript(session.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			tip, err := s.Session(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			boundary := tip.EntryTip + 1
			if mode == "retained" {
				boundary = warningID
			}
			if mode == "missing" {
				warning.RequestID++
			}
			if mode == "rollback" {
				trigger := fmt.Sprintf(`CREATE TRIGGER reject_warning BEFORE INSERT ON entries WHEN NEW.session_id != '%s' AND json_extract(NEW.content_json,'$.request_id')=%d BEGIN SELECT RAISE(ABORT, 'warning persistence rejected'); END`, session.ID, request)
				if _, err := s.DB.Exec(trigger); err != nil {
					t.Fatal(err)
				}
			}
			var before int
			if err := s.DB.QueryRow("SELECT count(*) FROM sessions").Scan(&before); err != nil {
				t.Fatal(err)
			}
			next, err := s.Continue(session.ID, "Handoff summary", archive, boundary, nil, time.Now(), &warning)
			if mode == "rollback" || mode == "missing" {
				if err == nil || mode == "rollback" && !strings.Contains(err.Error(), "warning persistence rejected") {
					t.Fatal("expected warning commit failure", err)
				}
				old, readErr := s.Session(session.ID)
				var after int
				countErr := s.DB.QueryRow("SELECT count(*) FROM sessions").Scan(&after)
				if readErr != nil || countErr != nil || old.ReadOnly || before != after {
					t.Fatal("failed warning commit left partial continuation", old, before, after, readErr, countErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			messages, err := s.Messages(next.ID)
			if err != nil || len(messages) != 2 || !reflect.DeepEqual(messages[1], warning) {
				t.Fatal("warning changed or was duplicated", messages, err)
			}
			entries, err := s.Branch(next.ID, 0)
			if err != nil || len(entries) != 2 || entries[1].EventSeq() != warningID || entries[1].TurnID != turn {
				t.Fatal("warning source provenance changed", entries, err)
			}
		})
	}
}
