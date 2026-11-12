package session

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/prompts"
)

const maxRuntimeNoticeBytes = history.MaxChildAnswerBytes

// noticePayloadFields is the explicit set of prose/stream fields supported by
// runtime notifications. All other fields remain structured metadata.
var noticePayloadFields = []string{"answer", "stdout", "stderr", "message", "error", "warning", "transcript_export_error"}

func utf8Prefix(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

func noticeWithFields(message llm.Message, fields map[string]json.RawMessage) (llm.Message, int, error) {
	body, err := json.Marshal(fields)
	if err != nil {
		return llm.Message{}, 0, err
	}
	message.Content = string(body)
	text, err := message.ModelContent()
	return message, len(text), err
}

// boundRuntimeNotice retains raw prefixes and source references, rather than
// replacing an oversized completion with an answerless notice. Sizing uses the
// final shared model projection, never JSON transport size or token estimates.
func boundRuntimeNotice(message llm.Message, sourceClipped []string, limit int) (llm.Message, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(message.Content), &fields); err != nil {
		return llm.Message{}, err
	}
	for _, key := range sourceClipped {
		fields[key+"_delivery_truncated"] = json.RawMessage("true")
	}
	candidate, size, err := noticeWithFields(message, fields)
	if err != nil {
		return llm.Message{}, err
	}
	if len(sourceClipped) == 0 && size <= limit {
		return candidate, nil
	}
	fields["delivery_truncated"] = json.RawMessage("true")
	// Retain the established source-backed inspector/export provenance contract.
	// output_omitted now means some output was omitted, not that answer is absent.
	fields["output_omitted"] = json.RawMessage("true")
	fields["details"], _ = json.Marshal(prompts.RuntimeEventHistoryDetails)

	payloads := make(map[string]string)
	for _, key := range noticePayloadFields {
		var text string
		if raw, ok := fields[key]; ok && len(raw) != 0 && raw[0] == '"' && json.Unmarshal(raw, &text) == nil {
			payloads[key] = text
		}
	}
	// Metadata cannot consume the entire delivery allowance. Normal generated
	// metadata fits unchanged; unusually long paths/labels retain prefixes with
	// source references. Routing IDs remain complete for provenance validation.
	_, metadataSize, err := sizedNotice(message, fields, payloads, 0)
	if err != nil {
		return llm.Message{}, err
	}
	if metadataSize > limit/2 {
		for key, raw := range fields {
			if _, payload := payloads[key]; payload || noticeIdentityField(key) {
				continue
			}
			var text string
			if json.Unmarshal(raw, &text) == nil && len(text) > 256 {
				fields[key], _ = json.Marshal(utf8Prefix(text, 256))
				fields[key+"_delivery_truncated"] = json.RawMessage("true")
			} else if len(raw) > 512 {
				fields[key] = json.RawMessage("null")
				fields[key+"_delivery_truncated"] = json.RawMessage("true")
			}
		}
	}
	// Water-filling shares the payload allowance equally, redistributing unused
	// room from shorter streams. Binary sizing accounts for metadata, delimiters
	// and UTF-8 boundaries. Keep only candidates actually within the byte limit.
	best, size, err := sizedNotice(message, fields, payloads, 0)
	if err != nil {
		return llm.Message{}, err
	}
	if size > limit {
		return llm.Message{}, fmt.Errorf(prompts.SessionNotificationMetadataLimit, limit)
	}
	low, high := 0, limit
	for low <= high {
		budget := low + (high-low)/2
		candidate, size, err := sizedNotice(message, fields, payloads, budget)
		if err != nil {
			return llm.Message{}, err
		}
		if size <= limit {
			best = candidate
			low = budget + 1
		} else {
			high = budget - 1
		}
	}
	return best, nil
}

func noticeIdentityField(key string) bool {
	switch key {
	case "type", "job_id", "child_id", "child_turn_id", "event_seq", "recovered_from_event_seq", "result_entry_id", "wakeup_id":
		return true
	}
	return false
}

func sizedNotice(message llm.Message, fields map[string]json.RawMessage, payloads map[string]string, budget int) (llm.Message, int, error) {
	copyFields := make(map[string]json.RawMessage, len(fields)+len(payloads))
	for key, raw := range fields {
		copyFields[key] = raw
	}
	type payload struct{ key, text string }
	ordered := make([]payload, 0, len(payloads))
	for key, text := range payloads {
		ordered = append(ordered, payload{key, text})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i].text) != len(ordered[j].text) {
			return len(ordered[i].text) < len(ordered[j].text)
		}
		return strings.Compare(ordered[i].key, ordered[j].key) < 0
	})
	for i, item := range ordered {
		allowance := budget / (len(ordered) - i)
		text := utf8Prefix(item.text, allowance)
		budget -= len(text)
		copyFields[item.key], _ = json.Marshal(text)
		if len(text) < len(item.text) {
			copyFields[item.key+"_delivery_truncated"] = json.RawMessage("true")
		}
	}
	return noticeWithFields(message, copyFields)
}
