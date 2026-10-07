package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"

	"ttc/internal/llm"
	"ttc/internal/prompts"
)

func headers(req *http.Request, t AccessTokens) {
	req.Header.Set("Authorization", "Bearer "+t.Access)
	req.Header.Set("ChatGPT-Account-ID", t.AccountID)
	req.Header.Set("originator", "ttc")
	req.Header.Set("User-Agent", "ttc/0.1")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
}

// binaryPart degrades only unavailable original bytes, not invalid references,
// cancellation or cache failures. Canonical messages remain unchanged.
func binaryPart(ctx context.Context, file llm.BinaryFile, model llm.ModelSpec, textKind string, resolve func(context.Context, llm.BinaryFile) (llm.BinaryPayload, error)) (map[string]any, error) {
	if resolve == nil {
		return nil, errors.New(prompts.OpenAIRequiresBinaryResolver)
	}
	payload, err := resolve(ctx, file)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		var unavailable *llm.UnavailableBinaryFileError
		if errors.As(err, &unavailable) {
			if file.MIMEType != "" {
				if _, err := supportedBinaryType(model, file.MIMEType, file.Bytes); err != nil {
					return nil, err
				}
			} else if !model.Images {
				return nil, errors.New(prompts.OpenAIImagesUnsupported)
			}
			return map[string]any{"type": textKind, "text": unavailable.Error()}, nil
		}
		return nil, err
	}
	format, err := supportedBinaryType(model, payload.MIMEType, len(payload.Data))
	if err != nil {
		return nil, err
	}
	url := "data:" + payload.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(payload.Data)
	if format.Kind == "image" {
		return map[string]any{"type": "input_image", "image_url": url}, nil
	}
	name := filepath.Base(file.Path)
	if file.Path == "" || name == "." || name == string(filepath.Separator) {
		return nil, errors.New(prompts.OpenAIDocumentFilenameRequired)
	}
	// Content-detected documents may be extensionless or have a misleading
	// suffix. Supply a parser-consistent transport filename without changing
	// the canonical local source path.
	ext := strings.ToLower(filepath.Ext(name))
	matched := false
	for _, candidate := range format.Extensions {
		if ext == "."+strings.ToLower(strings.TrimPrefix(candidate, ".")) {
			matched = true
			break
		}
	}
	if !matched && len(format.Extensions) > 0 {
		name += "." + strings.TrimPrefix(format.Extensions[0], ".")
	}
	return map[string]any{"type": "input_file", "filename": name, "file_data": url}, nil
}

func supportedBinaryType(model llm.ModelSpec, mt string, size int) (llm.BinaryFileType, error) {
	for _, format := range model.BinaryFileTypes() {
		if format.MIMEType != mt {
			continue
		}
		if format.Kind != "image" && format.Kind != "document" || format.MaxBytes <= 0 {
			return llm.BinaryFileType{}, errors.New(prompts.OpenAIInvalidBinaryMetadata)
		}
		if size > format.MaxBytes {
			return llm.BinaryFileType{}, fmt.Errorf(prompts.OpenAIBinaryLimit, mt, format.MaxBytes)
		}
		return format, nil
	}
	if strings.HasPrefix(mt, "image/") {
		return llm.BinaryFileType{}, errors.New(prompts.OpenAIImageFormatUnsupported)
	}
	return llm.BinaryFileType{}, fmt.Errorf(prompts.OpenAIBinaryMIMEUnsupported, mt)
}

func wire(ctx context.Context, req llm.Request, resolve func(context.Context, llm.BinaryFile) (llm.BinaryPayload, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Selection.Provider != "openai" || req.Selection.Model.RequestID() == "" {
		return nil, errors.New(prompts.OpenAISelectionRequired)
	}
	if req.ConversationID == "" || strings.IndexFunc(req.ConversationID, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return nil, errors.New(prompts.OpenAIConversationIdentityRequired)
	}
	input := []any{}
	for _, m := range llm.ContextFor(req.Selection, req.Messages) {
		for _, file := range m.Files {
			mt := file.MIMEType
			if mt != "" {
				if _, err := supportedBinaryType(req.Selection.Model, mt, file.Bytes); err != nil {
					return nil, err
				}
			} else if !req.Selection.Model.Images {
				return nil, errors.New(prompts.OpenAIImagesUnsupported)
			}
		}
		if m.State != nil {
			if len(m.Files) > 0 {
				return nil, errors.New(prompts.OpenAIReplayBinaryUnsupported)
			}
			items, err := replayItems(m)
			if err != nil {
				return nil, err
			}
			input = append(input, items...)
			continue
		}
		if m.Role == "tool" {
			var output any = m.Content
			if len(m.Files) > 0 {
				parts := []any{map[string]any{"type": "input_text", "text": m.Content}}
				for _, im := range m.Files {
					part, err := binaryPart(ctx, im, req.Selection.Model, "input_text", resolve)
					if err != nil {
						return nil, fmt.Errorf(prompts.OpenAIToolBinaryFile, m.CallID, err)
					}
					parts = append(parts, part)
				}
				output = parts
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.CallID, "output": output})
			continue
		}
		if m.Content != "" || len(m.Files) > 0 {
			content := []any{}
			kind := "input_text"
			if m.Role == "assistant" {
				kind = "output_text"
			}
			if m.Content != "" {
				content = append(content, map[string]any{"type": kind, "text": m.Content})
			}
			for _, im := range m.Files {
				part, err := binaryPart(ctx, im, req.Selection.Model, kind, resolve)
				if err != nil {
					return nil, fmt.Errorf(prompts.OpenAIMessageBinaryFile, m.Role, err)
				}
				content = append(content, part)
			}
			message := map[string]any{"type": "message", "role": m.Role, "content": content}
			if m.Role == "assistant" && m.Phase != "" {
				message["phase"] = m.Phase
			}
			input = append(input, message)
		}
		for _, c := range m.Calls {
			if !json.Valid(c.Arguments) {
				return nil, errors.New(prompts.OpenAIInvalidHistoryCallJSON)
			}
			input = append(input, map[string]any{"type": "function_call", "call_id": c.ID, "name": c.Name, "arguments": string(c.Arguments)})
		}
	}
	tools := []any{}
	if !req.NoTools {
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.Parameters, "strict": false})
		}
	}
	body := map[string]any{"model": req.Selection.Model.RequestID(), "service_tier": "default", "instructions": req.System, "input": input, "tools": tools, "tool_choice": "auto", "parallel_tool_calls": true, "stream": true, "store": false, "prompt_cache_key": req.ConversationID, "include": []string{"reasoning.encrypted_content"}}
	if req.Selection.Model.ServiceTier != "" {
		body["service_tier"] = req.Selection.Model.ServiceTier
	}
	if req.Selection.Variant != "" && (req.Selection.Variant != "none" || req.Selection.Model.SupportsReasoning) {
		body["reasoning"] = map[string]string{"effort": req.Selection.Variant, "summary": "auto"}
	}
	if req.NoTools {
		body["tool_choice"] = "none"
	}
	return json.Marshal(body)
}

type wireEvent struct {
	Type        string          `json:"type"`
	Delta       string          `json:"delta"`
	Item        json.RawMessage `json:"item"`
	ItemID      string          `json:"item_id"`
	OutputIndex *int            `json:"output_index"`
	Arguments   string          `json:"arguments"`
	Response    struct {
		ID          string     `json:"id"`
		ServiceTier string     `json:"service_tier"`
		Usage       *wireUsage `json:"usage"`
	} `json:"response"`
}

// Stream retries upstream failures within one bounded attempt budget.
// Committed failures return PartialError when another attempt is allowed;
// only the runtime may continue them, with a new request and retained partial history.
// Local validation, callback failures and cancellation never authorize retries.
// NoTools replies are buffered until completion, so their failed partial text is
// discarded and retried within the same budget without a runtime continuation.
// The subscription endpoint has no verified output cap; OutputTokens reserves context only.
func (a *Adapter) Stream(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
	maxAttempts := req.MaxAttempts
	if maxAttempts < 0 {
		return errors.New(prompts.OpenAIMaxAttemptsNonnegative)
	}
	if maxAttempts == 0 {
		maxAttempts = llm.DefaultMaxAttempts
	}
	if req.PriorAttempts < 0 || req.PriorAttempts >= maxAttempts {
		return errors.New(prompts.OpenAIPriorAttemptsRange)
	}
	body, e := wire(ctx, req, a.resolveBinary)
	if e != nil {
		return e
	}
	for attempt := req.PriorAttempts; attempt < maxAttempts; attempt++ {
		tokens, e := a.credentials(ctx)
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var transient *llm.TransientError
			if !errors.As(e, &transient) {
				return e
			}
			err := &llm.TransientError{Err: fmt.Errorf(prompts.OpenAIUpstreamAttemptFailed, attempt+1, maxAttempts, e)}
			if attempt == maxAttempts-1 {
				return err
			}
			if e = retryWait(ctx, emit, attempt, maxAttempts, err.Error(), credentialRetryAfter(e)); e != nil {
				return e
			}
			continue
		}
		h, e := http.NewRequestWithContext(ctx, "POST", a.BaseURL+"/responses", bytes.NewReader(body))
		if e != nil {
			return e
		}
		if (h.URL.Scheme != "http" && h.URL.Scheme != "https") || h.URL.Host == "" {
			return errors.New(prompts.OpenAIEndpointHTTPHostRequired)
		}
		headers(h, tokens)
		// ChatGPT uses session-id for cache affinity; keep it aligned with the body key.
		h.Header.Set("session-id", req.ConversationID)
		tier := req.Selection.Model.ServiceTier
		if tier == "" {
			tier = "default"
		}
		h.Header.Set("x-codex-routing-hint", "model="+req.Selection.Model.RequestID()+";tier="+tier)
		h.Header.Set("Content-Type", "application/json")
		h.Header.Set("Accept", "text/event-stream")
		for name, values := range h.Header {
			for _, value := range values {
				if !httpguts.ValidHeaderFieldValue(value) {
					return fmt.Errorf(prompts.OpenAIInvalidLocalHeader, name)
				}
			}
		}
		resp, upstreamErr := a.Client.Do(h)
		committed, callbackFailed := false, false
		retryAfter := ""
		if upstreamErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if final := finalSubscriptionFailure(upstreamErr); final != nil {
				return final
			}
			// A response alongside an error means Client.Do rejected a redirect
			// through local policy; retrying that policy cannot recover.
			if resp != nil {
				return errors.New(prompts.OpenAIRedirectPolicyFailed)
			}
			upstreamErr = transportFailure(upstreamErr)
		} else if resp.StatusCode != http.StatusOK {
			retryAfter = resp.Header.Get("Retry-After")
			upstreamErr = httpFailure(tokens.Access, resp)
			resp.Body.Close()
		} else {
			retryAfter = resp.Header.Get("Retry-After")
			// Naming and compaction publish only complete no-tools replies.
			// Buffer their text so a failed generation can be retried atomically,
			// rather than requiring a coding continuation or mixing two replies.
			var privateText strings.Builder
			var privateEvents []llm.StreamEvent
			committed, upstreamErr = parseStream(tokens.Access, resp.Body, func(event llm.StreamEvent) error {
				if req.NoTools && (event.Kind == "call" || event.Kind == "call_start") {
					return errors.New(prompts.OpenAINoToolsReturnedTool)
				}
				if req.NoTools {
					if event.Kind == "text" {
						privateText.WriteString(event.Text)
					} else {
						privateEvents = append(privateEvents, event)
					}
					return nil
				}
				err := emit(event)
				callbackFailed = callbackFailed || err != nil
				return finalCallbackError(err)
			})
			resp.Body.Close()
			if upstreamErr == nil {
				if req.NoTools {
					if privateText.Len() != 0 {
						if err := emit(llm.StreamEvent{Kind: "text", Text: privateText.String()}); err != nil {
							return finalCallbackError(err)
						}
					}
					for _, event := range privateEvents {
						if err := emit(event); err != nil {
							return finalCallbackError(err)
						}
					}
				}
				return nil
			}
			if req.NoTools {
				committed = false // Buffered output has not reached a consumer.
			}
			if !callbackFailed {
				upstreamErr = failureWithRequestID(upstreamErr, redactAccessToken(resp.Header.Get("x-request-id"), tokens.Access))
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if callbackFailed {
			return upstreamErr
		}
		if final := finalSubscriptionFailure(upstreamErr); final != nil {
			return final
		}
		// Every upstream failure shares this budget, regardless of its code or
		// whether it came from HTTP, SSE, transport or response validation.
		err := &llm.TransientError{Err: fmt.Errorf(prompts.OpenAIUpstreamAttemptFailed, attempt+1, maxAttempts, upstreamErr)}
		if attempt == maxAttempts-1 {
			return err
		}
		if committed {
			return &llm.PartialError{Err: err, Retry: retryMetadata(attempt, maxAttempts, err.Error(), retryDelay(attempt, retryAfter, time.Now()))}
		}
		if e = retryWait(ctx, emit, attempt, maxAttempts, err.Error(), retryAfter); e != nil {
			return e
		}
	}
	return errors.New(prompts.OpenAIAttemptsExhausted)
}

var errStreamLost = errors.New(prompts.OpenAIStreamInterrupted)
var errSubscriptionCertificate = errors.New(prompts.OpenAISubscriptionCertificateFailed)
var errInvalidToolArguments = errors.New(prompts.OpenAICompletedArgumentsNotObject)
var errConflictingToolArguments = errors.New(prompts.OpenAICompletedArgumentsConflict)

// finalSubscriptionFailure preserves final categories without exposing arbitrary
// transport or reader error values. The outer request context may still be live.
func finalSubscriptionFailure(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, errSubscriptionCertificate) || invalidCertificate(err) {
		return errSubscriptionCertificate
	}
	return nil
}

func parseStream(accessToken string, reader io.Reader, emit func(llm.StreamEvent) error) (bool, error) {
	scan := bufio.NewScanner(reader)
	scan.Buffer(make([]byte, 4096), 8<<20)
	data := []string{}
	committed, complete := false, false
	calls := map[int]*streamedCall{}
	callIDs := map[string]bool{}
	itemIDs := map[string]bool{}
	nativeItems := map[int]json.RawMessage{}
	retainedBytes := 0
	var streamedText strings.Builder
	eventBytes := 0
	consume := func() error {
		if len(data) == 0 {
			return nil
		}
		b := strings.Join(data, "\n")
		data = nil
		eventBytes = 0
		if b == "[DONE]" {
			return nil
		}
		var event wireEvent
		if e := json.Unmarshal([]byte(b), &event); e != nil {
			// Preserve terminal diagnostics even if other response fields have
			// unexpected types; the failure decoder does not consume output.
			var envelope struct {
				Type string `json:"type"`
			}
			if json.Unmarshal([]byte(b), &envelope) == nil && (envelope.Type == "error" || envelope.Type == "response.failed" || envelope.Type == "response.incomplete") {
				return streamFailure(accessToken, []byte(b), envelope.Type)
			}
			return errors.New(prompts.OpenAIInvalidStreamJSON)
		}
		var out *llm.StreamEvent
		switch event.Type {
		case "response.output_text.delta", "response.refusal.delta":
			retainedBytes += len(event.Delta)
			streamedText.WriteString(event.Delta)
			out = &llm.StreamEvent{Kind: "text", Text: event.Delta}
		case "response.output_item.added":
			var item functionItem
			if err := json.Unmarshal(event.Item, &item); err != nil {
				return errors.New(prompts.OpenAIInvalidAddedItem)
			}
			if item.Type == "function_call" {
				if event.OutputIndex == nil || *event.OutputIndex < 0 || item.ID == "" || item.CallID == "" || item.Name == "" || calls[*event.OutputIndex] != nil || nativeItems[*event.OutputIndex] != nil || callIDs[item.CallID] || itemIDs[item.ID] {
					return errors.New(prompts.OpenAIInvalidToolAnnouncement)
				}
				c := &streamedCall{item: item}
				c.arguments.WriteString(item.Arguments)
				calls[*event.OutputIndex] = c
				callIDs[item.CallID] = true
				itemIDs[item.ID] = true
				retainedBytes += len(item.Arguments) + len(item.ID) + len(item.CallID) + len(item.Name)
				out = &llm.StreamEvent{Kind: "call_start", CallStart: &llm.ToolStart{ID: item.CallID, Name: item.Name}}
			}
		case "response.function_call_arguments.delta", "response.function_call_arguments.done":
			if event.OutputIndex == nil {
				return errors.New(prompts.OpenAIToolArgumentsMissingIndex)
			}
			c := calls[*event.OutputIndex]
			if c == nil || c.item.ID != event.ItemID || c.finished || c.argumentsDone {
				return errors.New(prompts.OpenAIToolArgumentsInvalidLifecycle)
			}
			if event.Type == "response.function_call_arguments.delta" {
				if c.arguments.Len()+len(event.Delta) > 8<<20 {
					return errors.New(prompts.OpenAIToolArgumentsLimit)
				}
				c.arguments.WriteString(event.Delta)
				c.segments++
				c.streamedBytes += len(event.Delta)
				retainedBytes += len(event.Delta)
				now := time.Now()
				if c.segments == 1 || now.Sub(c.lastProgress) >= 100*time.Millisecond {
					out = c.progressEvent()
					c.lastProgress, c.reportedSegments = now, c.segments
				}
			} else {
				if !validArguments(event.Arguments) {
					return errInvalidToolArguments
				}
				// The finalized argument event is authoritative. Deltas are for live
				// progress and can differ from the completed value.
				c.arguments.Reset()
				c.arguments.WriteString(event.Arguments)
				c.argumentsDone = true
				if c.reportedSegments != c.segments {
					out = c.progressEvent()
					c.reportedSegments = c.segments
				}
			}
		case "response.output_item.done":
			var item functionItem
			if e := json.Unmarshal(event.Item, &item); e != nil {
				return errors.New(prompts.OpenAIInvalidCompletedItem)
			}
			if item.Type == "function_call" {
				if event.OutputIndex == nil {
					return errors.New(prompts.OpenAICompletedToolMissingIndex)
				}
				c := calls[*event.OutputIndex]
				if c == nil || c.finished || !c.argumentsDone || c.item.ID != item.ID || c.item.CallID != item.CallID || c.item.Name != item.Name {
					return errors.New(prompts.OpenAICompletedToolConflict)
				}
				if c.arguments.String() != item.Arguments {
					return errConflictingToolArguments
				}
				c.finished = true
			} else if item.Type != "reasoning" && item.Type != "message" {
				return errors.New(prompts.OpenAIUnsupportedOutputItem)
			}
			if event.OutputIndex == nil || *event.OutputIndex < 0 || nativeItems[*event.OutputIndex] != nil || item.Type != "function_call" && calls[*event.OutputIndex] != nil {
				return errors.New(prompts.OpenAIInvalidCompletedOutputIndex)
			}
			nativeItems[*event.OutputIndex] = append(json.RawMessage(nil), event.Item...)
			retainedBytes += len(event.Item)
			// Native items stay private until response.completed. In particular,
			// buffered reasoning alone must not prevent a safe transport retry.
		case "response.completed":
			indices := make([]int, 0, len(calls))
			for _, c := range calls {
				if !c.finished {
					return errors.New(prompts.OpenAIUnfinishedToolArguments)
				}
			}
			for index := range nativeItems {
				indices = append(indices, index)
			}
			sort.Ints(indices)
			reply := llm.Message{Role: "assistant", Content: streamedText.String(), State: &llm.ReplayState{Version: replayVersion}}
			for _, index := range indices {
				reply.State.Items = append(reply.State.Items, nativeItems[index])
				if c := calls[index]; c != nil {
					reply.Calls = append(reply.Calls, llm.ToolCall{ID: c.item.CallID, Name: c.item.Name, Arguments: json.RawMessage(c.arguments.String())})
				}
			}
			if len(indices) > 0 || reply.Content != "" || len(calls) > 0 {
				if _, err := replayItems(reply); err != nil {
					return err
				}
			}
			for _, index := range indices {
				var native struct {
					Type  string
					Phase string
				}
				if err := json.Unmarshal(nativeItems[index], &native); err != nil {
					return errors.New(prompts.OpenAIInvalidCompletedItemMetadata)
				}
				if native.Type == "message" && native.Phase != "" {
					if err := emit(llm.StreamEvent{Kind: "phase", Phase: native.Phase}); err != nil {
						return err
					}
				}
				if err := emit(llm.StreamEvent{Kind: "state", StateVersion: replayVersion, StateItem: nativeItems[index]}); err != nil {
					return err
				}
			}
			indices = indices[:0]
			for index := range calls {
				indices = append(indices, index)
			}
			sort.Ints(indices)
			for _, index := range indices {
				c := calls[index]
				if err := emit(llm.StreamEvent{Kind: "call", Call: &llm.ToolCall{ID: c.item.CallID, Name: c.item.Name, Arguments: json.RawMessage(c.arguments.String())}}); err != nil {
					return err
				}
			}
			complete = true
			out = &llm.StreamEvent{Kind: "completed", Usage: event.Response.Usage.normalized(), ResponseID: event.Response.ID, ServiceTier: event.Response.ServiceTier}
		case "response.failed", "response.incomplete", "error":
			return streamFailure(accessToken, []byte(b), event.Type)
		}
		if retainedBytes > 32<<20 {
			return errors.New(prompts.OpenAIOutputLimit)
		}
		if out != nil {
			committed = true
			if e := emit(*out); e != nil {
				return e
			}
		}
		return nil
	}
	for scan.Scan() {
		line := scan.Text()
		if line == "" {
			if e := consume(); e != nil {
				return committed, e
			}
			if complete {
				return committed, nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			eventBytes += len(line)
			if eventBytes > 8<<20 {
				return committed, errors.New(prompts.OpenAIStreamEventLimit)
			}
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if e := scan.Err(); e != nil {
		if final := finalSubscriptionFailure(e); final != nil {
			return committed, final
		}
		if errors.Is(e, bufio.ErrTooLong) {
			return committed, errors.New(prompts.OpenAIStreamLineLimit)
		}
		return committed, errStreamLost
	}
	if e := consume(); e != nil {
		return committed, e
	}
	if !complete {
		return committed, errStreamLost
	}
	return committed, nil
}

type functionItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type streamedCall struct {
	item                    functionItem
	arguments               strings.Builder
	segments, streamedBytes int
	reportedSegments        int
	lastProgress            time.Time
	argumentsDone, finished bool
}

func (c *streamedCall) progressEvent() *llm.StreamEvent {
	return &llm.StreamEvent{Kind: "call_progress", CallProgress: &llm.ToolProgress{ID: c.item.CallID, Name: c.item.Name, Segments: c.segments, Bytes: c.streamedBytes}}
}

func validArguments(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "{") && json.Valid([]byte(text))
}
