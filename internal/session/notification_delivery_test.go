package session

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"ttc/internal/llm"
)

func boundedNoticeFixture(t *testing.T, fields map[string]any) (llm.Message, map[string]json.RawMessage) {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	message, err := boundRuntimeNotice(llm.Message{Type: llm.TypeRuntimeNotification, Content: string(body), EventSeq: 42}, nil, maxRuntimeNoticeBytes)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := message.ModelContent()
	if err != nil || len(projected) > maxRuntimeNoticeBytes || !utf8.ValidString(projected) {
		t.Fatalf("invalid formatted delivery: bytes=%d err=%v", len(projected), err)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal([]byte(message.Content), &result); err != nil {
		t.Fatal(err)
	}
	return message, result
}

func noticeString(t *testing.T, fields map[string]json.RawMessage, key string) string {
	t.Helper()
	var text string
	if err := json.Unmarshal(fields[key], &text); err != nil {
		t.Fatal(key, err)
	}
	return text
}

func TestNoticeByteLimitPreservesAnswerAndFailureEvidence(t *testing.T) {
	answer := strings.Repeat("界\"\n<<<ttc-123 ", 2000)
	_, fields := boundedNoticeFixture(t, map[string]any{
		"type": "child_turn_finished", "child_id": "main/child_123", "job_id": "job_1",
		"assignment_status": "failed", "result_entry_id": 9007199254740993,
		"answer": answer, "answer_truncated": true, "warning": "Partial work; inspect artifacts.",
		"error": "test failure", "transcript_path": "/tmp/failure.md", "event_seq": 42,
	})
	prefix := noticeString(t, fields, "answer")
	if prefix == "" || !strings.HasPrefix(answer, prefix) || !utf8.ValidString(prefix) || fields["answer_delivery_truncated"] == nil || fields["answer_truncated"] == nil {
		t.Fatal("answer prefix or distinct truncation flags lost", fields)
	}
	if noticeString(t, fields, "warning") != "Partial work; inspect artifacts." || noticeString(t, fields, "error") != "test failure" || string(fields["result_entry_id"]) != "9007199254740993" || noticeString(t, fields, "transcript_path") != "/tmp/failure.md" {
		t.Fatal("failure references or outcome lost", fields)
	}
}

func TestNoticeStreamsShareOneBudgetAndRedistribute(t *testing.T) {
	for _, short := range []bool{false, true} {
		stderr := strings.Repeat("error!\n", 2000)
		if short {
			stderr = "important failure"
		}
		_, fields := boundedNoticeFixture(t, map[string]any{
			"type": "job_exit", "job_id": "job_1", "stdout": strings.Repeat("output\n", 2000), "stderr": stderr,
			"exit_code": 1, "stdout_selection": map[string]any{"omitted_bytes": 99}, "truncated": true,
		})
		out, diagnostic := noticeString(t, fields, "stdout"), noticeString(t, fields, "stderr")
		if out == "" || diagnostic == "" || fields["stdout_delivery_truncated"] == nil || fields["stdout_selection"] == nil || string(fields["truncated"]) != "true" {
			t.Fatal("stream or capture metadata lost", fields)
		}
		if short {
			if diagnostic != stderr || len(out) < 3000 || fields["stderr_delivery_truncated"] != nil {
				t.Fatal("short diagnostic not preserved or unused budget not redistributed", len(out), diagnostic)
			}
		} else if difference := len(out) - len(diagnostic); difference < -1 || difference > 1 {
			t.Fatal("stream budget not shared equally", len(out), len(diagnostic))
		}
	}
}

func TestNoticeProjectionDoesNotChargeJSONEscaping(t *testing.T) {
	answer := strings.Repeat("\"\\\n", 1400)
	_, fields := boundedNoticeFixture(t, map[string]any{"type": "child_turn_finished", "answer": answer, "child_id": "main/child_1"})
	if noticeString(t, fields, "answer") != answer || fields["delivery_truncated"] != nil {
		t.Fatal("JSON escaping consumed raw-text delivery allowance")
	}
}

func TestNoticeShortAnswerDoesNotUseHalfTokenBudget(t *testing.T) {
	answer := strings.Repeat("a", 3500)
	_, fields := boundedNoticeFixture(t, map[string]any{"type": "child_turn_finished", "answer": answer, "child_id": "main/child_1"})
	if noticeString(t, fields, "answer") != answer || fields["output_omitted"] != nil {
		t.Fatal("modest answer was needlessly omitted")
	}
}

func TestNoticeBoundingPreservesNullPayloadFields(t *testing.T) {
	_, fields := boundedNoticeFixture(t, map[string]any{
		"type": "job_exit", "job_id": "job", "stdout": strings.Repeat("x", 10000), "stderr": nil,
	})
	if string(fields["stderr"]) != "null" {
		t.Fatal("null metadata became an empty stream", fields["stderr"])
	}
}
