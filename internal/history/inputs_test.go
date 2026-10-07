package history

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/llm"
)

type countingHistoryReader struct {
	historyReader
	queries, rowQueries int
}

func (q *countingHistoryReader) Query(query string, args ...any) (*sql.Rows, error) {
	q.queries++
	return q.historyReader.Query(query, args...)
}

func (q *countingHistoryReader) QueryRow(query string, args ...any) *sql.Row {
	q.rowQueries++
	return q.historyReader.QueryRow(query, args...)
}

func TestHumanProvenanceQueryCountIsConstant(t *testing.T) {
	for _, count := range []int{1, 64, 1200} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, session, turn, _ := historyFixture(t)
			for i := 1; i < count; i++ {
				if _, err := s.Append(session.ID, turn, "main", "message", "user", true, llm.Message{Role: "user", Content: "queued", InputSource: "queue", InputTimeMS: 1}); err != nil {
					t.Fatal(err)
				}
			}
			check := func(reader historyReader) []llm.Message {
				t.Helper()
				q := &countingHistoryReader{historyReader: reader}
				messages, err := messagesWith(q, session.ID)
				if err != nil || len(messages) != count {
					t.Fatal(len(messages), err)
				}
				// One tip lookup, one ancestry read, and one batch provenance read.
				// In particular, no original-human QueryRow calls may scale with count.
				if q.queries != 2 || q.rowQueries != 1 {
					t.Fatalf("%d humans used %d queries and %d row queries; want 2 and 1", count, q.queries, q.rowQueries)
				}
				for i, message := range messages {
					wantSource := "queue"
					if i == 0 {
						wantSource = "normal"
					}
					if message.InputSource != wantSource || message.InputTimeMS <= 1 {
						t.Fatal("incorrect original metadata", message)
					}
				}
				return messages
			}
			outside := check(s.DB)
			if err := s.transact(func(tx *sql.Tx) error {
				if inside := check(tx); !reflect.DeepEqual(inside, outside) {
					t.Fatal("transaction projection differs from database projection")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBatchedHumanProvenanceSeesUncommittedAdmissions(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	before, err := s.Messages(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("rollback fixture")
	err = s.transact(func(tx *sql.Tx) error {
		content, err := json.Marshal(llm.Message{Role: "user", Content: "pending", InputSource: "queue", InputTimeMS: 1})
		if err != nil {
			return err
		}
		id, err := appendTx(tx, session.ID, turn, "main", "message", "user", true, content, 0)
		if err != nil {
			return err
		}
		var committedMS int64
		if err := tx.QueryRow("SELECT created_ms FROM entries WHERE id=?", id).Scan(&committedMS); err != nil {
			return err
		}
		q := &countingHistoryReader{historyReader: tx}
		messages, err := messagesWith(q, session.ID)
		if err != nil || len(messages) != len(before)+1 {
			t.Fatal(messages, err)
		}
		last := messages[len(messages)-1]
		if last.InputSource != "queue" || last.InputTimeMS != committedMS || q.queries != 2 || q.rowQueries != 1 {
			t.Fatal("pending admission lost authoritative metadata", last, committedMS, q)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	after, err := s.Messages(session.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("projection escaped rollback", after, err)
	}
}

func TestBatchedHumanProvenanceUsesOriginalNotCopyMetadata(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	original, err := s.Entry(session.EntryTip)
	if err != nil {
		t.Fatal(err)
	}
	var copyID int64
	if err := s.transact(func(tx *sql.Tx) error {
		content, err := json.Marshal(llm.Message{Role: "user", Content: "copied", InputSource: "invalid caller metadata", InputTimeMS: 1})
		if err != nil {
			return err
		}
		copyID, err = appendTx(tx, session.ID, turn, "main", "message", "user", true, content, original.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	messages, err := s.Messages(session.ID)
	if err != nil || len(messages) != 2 || messages[1].InputSource != "normal" || messages[1].InputTimeMS != original.CreatedMS {
		t.Fatal("copy overrode original metadata", messages, err)
	}
	copy, err := s.Entry(copyID)
	if err != nil || copy.EventSeq() != original.ID {
		t.Fatal("copy lost original entry identity", copy, err)
	}
	if _, err := s.DB.Exec(`UPDATE entries SET content_json=json_set(content_json,'$.input_source','invalid original source') WHERE id=?`, original.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Messages(session.ID); err == nil || !strings.Contains(err.Error(), "invalid source") {
		t.Fatal("accepted invalid original source", err)
	}
}

func TestHumanProvenanceRejectsMissingOriginal(t *testing.T) {
	entry := Entry{ID: 2, Source: 1, Visible: true, Actor: "main", Kind: "message", Role: "user"}
	if _, err := enrichInput(nil, entry, llm.Message{Role: "user"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("accepted missing original", err)
	}
	if _, err := enrichInput(nil, entry, llm.Message{Role: "user", Runtime: true}); err != nil {
		t.Fatal("runtime notice required human provenance", err)
	}
}
