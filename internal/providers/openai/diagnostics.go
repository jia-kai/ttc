package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"ttc/internal/prompts"
)

const (
	failureBodyLimit       = 8 * 1024
	failureStreamLimit     = 64 * 1024 // Bound diagnostic parsing separately from the SSE event limit.
	failureDiagnosticLimit = 4096
	failureRequestIDLimit  = 256
	// Leave room for an optional request ID without exceeding the diagnostic cap.
	failureDetailsLimit = failureDiagnosticLimit - failureRequestIDLimit - 16
)

// streamFailure diagnoses terminal SSE events without assigning retry policy.
func streamFailure(accessToken string, raw []byte, kind string) error {
	details := prompts.OpenAIEventDetailsOmitted
	if len(raw) <= failureStreamLimit {
		details = failureDetails(accessToken, raw, kind)
	}
	text := safeFailureValue(kind, 256, accessToken) + "; " + details
	return fmt.Errorf(prompts.OpenAIStreamTerminated, safeFailureText(text, failureDetailsLimit))
}

// httpFailure consumes at most failureBodyLimit+1 bytes; the caller owns Close.
// Only documented JSON error fields and x-request-id may contribute values.
func httpFailure(accessToken string, resp *http.Response) error {
	if resp == nil {
		return errors.New(prompts.OpenAIMissingHTTPResponse)
	}
	var raw []byte
	var readErr error
	if resp.Body != nil {
		raw, readErr = io.ReadAll(io.LimitReader(resp.Body, failureBodyLimit+1))
	}
	if final := finalSubscriptionFailure(readErr); final != nil {
		return final
	}
	truncated := len(raw) > failureBodyLimit
	if truncated {
		raw = raw[:failureBodyLimit]
	}
	text := failureDetails(accessToken, raw, "")
	if truncated {
		text = prompts.OpenAIBodyTruncatedPrefix + text
	}
	if readErr != nil {
		// Reader errors may themselves contain credentials or raw upstream data.
		text = prompts.OpenAIBodyReadFailedPrefix + text
	}
	err := fmt.Errorf(prompts.OpenAIResponseHTTP, resp.StatusCode, safeFailureText(text, failureDetailsLimit))
	return failureWithRequestID(err, redactAccessToken(resp.Header.Get("x-request-id"), accessToken))
}

// failureWithRequestID retains the cause for errors.Is/As. The supplied error
// should already be a safe diagnostic; only the header value is sanitized here.
func failureWithRequestID(err error, id string) error {
	if err == nil {
		return nil
	}
	id = safeFailureText(id, failureRequestIDLimit)
	if id == "" {
		return err
	}
	return fmt.Errorf(prompts.OpenAIFailureRequestID, err, id)
}

func failureDetails(accessToken string, raw []byte, kind string) string {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return prompts.OpenAIErrorDetailsBodyAbsent
	}
	if !json.Valid(raw) {
		return prompts.OpenAIErrorDetailsBodyInvalidJSON
	}
	root := failureObject(raw)
	if root == nil {
		return fmt.Sprintf(prompts.OpenAIErrorDetailsShape, failureShape(accessToken, raw, 0))
	}
	var parts []string
	response := failureObject(root["response"])
	// Response identity is useful even when no error details are supplied.
	if id := failureString(response["id"]); id != "" {
		parts = append(parts, "response_id="+safeFailureValue(id, 256, accessToken))
	} else if id := failureString(root["response_id"]); id != "" {
		parts = append(parts, "response_id="+safeFailureValue(id, 256, accessToken))
	}
	details := 0
	malformed := false
	appendFields := func(object map[string]json.RawMessage, incomplete, eventFields bool) {
		for _, field := range []string{"type", "code", "message", "param", "reason"} {
			if field == "reason" && !incomplete {
				continue
			}
			rawValue := object[field]
			var value string
			if len(rawValue) != 0 && json.Unmarshal(rawValue, &value) != nil {
				malformed = true
			}
			if value == "" || field == "type" && eventFields && value == kind {
				continue
			}
			label := field
			if field == "param" {
				label = prompts.OpenAIDiagnosticParameter
			}
			limit := 256
			if field == "message" {
				limit = 2048
			}
			value = safeFailureValue(value, limit, accessToken)
			if value == "" {
				continue
			}
			parts = append(parts, label+"="+value)
			details++
		}
	}
	appendFields(failureObject(response["error"]), false, false)
	appendFields(failureObject(response["incomplete_details"]), true, false)
	appendFields(failureObject(root["error"]), false, false)
	appendFields(root, false, true)
	if details == 0 {
		parts = append(parts, prompts.OpenAIErrorDetailsMissing)
	}
	if details == 0 || malformed {
		parts = append(parts, fmt.Sprintf(prompts.OpenAIDiagnosticShape, failureShape(accessToken, raw, 0)))
	}
	return strings.Join(parts, "; ")
}

func failureObject(raw json.RawMessage) map[string]json.RawMessage {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	return object
}

func failureString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

// failureShape reports bounded field names and JSON types, never scalar values
// or array contents. In particular response output and encrypted reasoning are
// not traversed. A depth cap keeps unfamiliar object trees compact.
func failureShape(accessToken string, raw json.RawMessage, depth int) string {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return prompts.OpenAIDiagnosticAbsent
	}
	switch raw[0] {
	case '{':
		if depth >= 2 {
			return prompts.OpenAIDiagnosticObject
		}
		object := failureObject(raw)
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		fields := make([]string, 0, min(len(keys), 12)+1)
		for _, key := range keys[:min(len(keys), 12)] {
			name, _ := json.Marshal(safeFailureValue(key, 80, accessToken))
			fields = append(fields, string(name)+":"+failureShape(accessToken, object[key], depth+1))
		}
		if len(keys) > 12 {
			fields = append(fields, prompts.OpenAIDiagnosticTruncated)
		}
		return fmt.Sprintf(prompts.OpenAIDiagnosticObjectFields, strings.Join(fields, ","))
	case '[':
		return prompts.OpenAIDiagnosticArray
	case '"':
		return prompts.OpenAIDiagnosticString
	case 't', 'f':
		return prompts.OpenAIDiagnosticBoolean
	case 'n':
		return prompts.OpenAIDiagnosticNull
	default:
		return prompts.OpenAIDiagnosticNumber
	}
}

// safeFailureValue redacts decoded values before sanitization and truncation,
// so JSON escapes and tokens crossing the field limit cannot evade redaction.
func safeFailureValue(text string, limit int, accessToken string) string {
	return safeFailureText(redactAccessToken(text, accessToken), limit)
}

func redactAccessToken(text, accessToken string) string {
	if accessToken != "" {
		text = strings.ReplaceAll(text, accessToken, prompts.OpenAIDiagnosticRedacted)
	}
	return text
}

// safeFailureText produces one control-free line of valid, bounded UTF-8.
// Format controls include all bidi overrides, isolates and direction marks.
func safeFailureText(text string, limit int) string {
	var out strings.Builder
	space := false
	for _, r := range text {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
			space = out.Len() > 0
			continue
		}
		if space {
			out.WriteByte(' ')
			space = false
		}
		out.WriteRune(r)
		if out.Len() > limit {
			value := out.String()[:limit-len(prompts.OpenAIDiagnosticTruncated)]
			for !utf8.ValidString(value) {
				value = value[:len(value)-1]
			}
			return value + prompts.OpenAIDiagnosticTruncated
		}
	}
	return out.String()
}
