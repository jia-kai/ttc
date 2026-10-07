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
	"unicode"
	"unicode/utf8"

	"ttc/internal/llm"
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
		return nil, errors.New("OpenAI binary input requires a configured resolver")
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
				return nil, errors.New("model does not support images")
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
		return nil, errors.New("document attachment requires a filename")
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
			return llm.BinaryFileType{}, errors.New("invalid model binary format metadata")
		}
		if size > format.MaxBytes {
			return llm.BinaryFileType{}, fmt.Errorf("binary file %s exceeds model limit of %d bytes", mt, format.MaxBytes)
		}
		return format, nil
	}
	if strings.HasPrefix(mt, "image/") {
		return llm.BinaryFileType{}, errors.New("model does not support images in this format")
	}
	return llm.BinaryFileType{}, fmt.Errorf("model does not support binary file MIME type %q", mt)
}

func wire(ctx context.Context, req llm.Request, resolve func(context.Context, llm.BinaryFile) (llm.BinaryPayload, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Selection.Provider != "openai" || req.Selection.Model.RequestID() == "" {
		return nil, errors.New("OpenAI adapter requires an OpenAI model selection")
	}
	if req.ConversationID == "" || strings.IndexFunc(req.ConversationID, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return nil, errors.New("OpenAI adapter requires a nonempty printable ASCII conversation identity without whitespace")
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
				return nil, errors.New("model does not support images")
			}
		}
		if m.State != nil {
			if len(m.Files) > 0 {
				return nil, errors.New("OpenAI replay state does not support canonical binary files")
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
						return nil, fmt.Errorf("tool output %q binary file: %w", m.CallID, err)
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
					return nil, fmt.Errorf("%s message binary file: %w", m.Role, err)
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
				return nil, errors.New("invalid call JSON in history")
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
	Code        string          `json:"code"` // Error events use top-level fields.
	Message     string          `json:"message"`
	Param       string          `json:"param"`
	Response    struct {
		ID          string     `json:"id"`
		ServiceTier string     `json:"service_tier"`
		Usage       *wireUsage `json:"usage"`
		Error       *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Incomplete *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
}

// Stream retries uncommitted transient failures until cancellation or MaxAttempts.
// Committed transient failures return PartialError when another attempt is allowed;
// only the runtime may continue them, with a new request and retained partial history.
// The subscription endpoint has no verified output cap; OutputTokens reserves context only.
func (a *Adapter) Stream(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
	maxAttempts := req.MaxAttempts
	if maxAttempts < 0 {
		return errors.New("max attempts must be nonnegative")
	}
	if req.PriorAttempts < 0 || maxAttempts > 0 && req.PriorAttempts >= maxAttempts {
		return errors.New("prior attempts must be nonnegative and below max attempts")
	}
	body, e := wire(ctx, req, a.resolveBinary)
	if e != nil {
		return e
	}
	for attempt := req.PriorAttempts; maxAttempts == 0 || attempt < maxAttempts; attempt++ {
		tokens, e := a.credentials(ctx)
		if e != nil {
			var transient *llm.TransientError
			if !errors.As(e, &transient) || maxAttempts > 0 && attempt == maxAttempts-1 {
				return e
			}
			if e = retryWait(ctx, emit, attempt, maxAttempts, transient.Error(), ""); e != nil {
				return e
			}
			continue
		}
		h, e := http.NewRequestWithContext(ctx, "POST", a.BaseURL+"/responses", bytes.NewReader(body))
		if e != nil {
			return e
		}
		if (h.URL.Scheme != "http" && h.URL.Scheme != "https") || h.URL.Host == "" {
			return errors.New("subscription endpoint requires an HTTP(S) URL with a host")
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
		resp, e := a.Client.Do(h)
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if invalidCertificate(e) {
				return errors.New("subscription TLS certificate verification failed")
			}
			if maxAttempts > 0 && attempt == maxAttempts-1 {
				return &llm.TransientError{Err: errors.New("subscription transport failed before response")}
			}
			if e = retryWait(ctx, emit, attempt, maxAttempts, "transport failed before response", ""); e != nil {
				return e
			}
			continue
		}
		if resp.StatusCode != 200 {
			status := resp.StatusCode
			resp.Body.Close()
			if (status == 429 || status >= 500 && status <= 599) && (maxAttempts == 0 || attempt < maxAttempts-1) {
				if e = retryWait(ctx, emit, attempt, maxAttempts, fmt.Sprintf("HTTP %d", status), resp.Header.Get("Retry-After")); e != nil {
					return e
				}
				continue
			}
			err := fmt.Errorf("subscription response HTTP %d", status)
			if status == 429 || status >= 500 && status <= 599 {
				return &llm.TransientError{Err: err}
			}
			return err
		}
		callbackFailed := false
		committed, err := parseStream(resp.Body, func(event llm.StreamEvent) error {
			if req.NoTools && (event.Kind == "call" || event.Kind == "call_start") {
				return errors.New("OpenAI returned a tool to a no-tools request")
			}
			err := emit(event)
			callbackFailed = callbackFailed || err != nil
			return finalCallbackError(err)
		})
		resp.Body.Close()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		reason := "temporary stream failure before output"
		if retryableToolArguments(err) && !callbackFailed {
			reason = err.Error()
			err = &llm.TransientError{Err: err}
		}
		if errors.Is(err, errStreamLost) && !callbackFailed {
			err = &llm.TransientError{Err: err}
			reason = "stream interrupted before output"
		}
		var transient *llm.TransientError
		if callbackFailed || maxAttempts > 0 && attempt == maxAttempts-1 || !errors.As(err, &transient) {
			return err
		}
		if committed {
			partialReason := "stream interrupted after partial output"
			if retryableToolArguments(err) {
				partialReason = reason
			}
			return &llm.PartialError{Err: err, Retry: retryMetadata(attempt, maxAttempts, partialReason, retryDelay(attempt, "", time.Now()))}
		}
		if e = retryWait(ctx, emit, attempt, maxAttempts, reason, ""); e != nil {
			return e
		}
	}
	return errors.New("request attempts exhausted")
}

var errStreamLost = errors.New("Responses stream interrupted before completion")
var errInvalidToolArguments = errors.New("completed tool arguments are not a JSON object")
var errConflictingToolArguments = errors.New("completed tool disagrees with finalized arguments")

func retryableToolArguments(err error) bool {
	return errors.Is(err, errInvalidToolArguments) || errors.Is(err, errConflictingToolArguments)
}

func parseStream(reader io.Reader, emit func(llm.StreamEvent) error) (bool, error) {
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
			return errors.New("invalid Responses stream JSON")
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
				return errors.New("invalid added Responses item")
			}
			if item.Type == "function_call" {
				if event.OutputIndex == nil || *event.OutputIndex < 0 || item.ID == "" || item.CallID == "" || item.Name == "" || calls[*event.OutputIndex] != nil || nativeItems[*event.OutputIndex] != nil || callIDs[item.CallID] || itemIDs[item.ID] {
					return errors.New("invalid or duplicate streamed tool announcement")
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
				return errors.New("tool arguments missing output index")
			}
			c := calls[*event.OutputIndex]
			if c == nil || c.item.ID != event.ItemID || c.finished || c.argumentsDone {
				return errors.New("tool arguments have unknown identity or invalid lifecycle")
			}
			if event.Type == "response.function_call_arguments.delta" {
				if c.arguments.Len()+len(event.Delta) > 8<<20 {
					return errors.New("streamed tool arguments exceed 8 MiB")
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
				return e
			}
			if item.Type == "function_call" {
				if event.OutputIndex == nil {
					return errors.New("completed tool missing output index")
				}
				c := calls[*event.OutputIndex]
				if c == nil || c.finished || !c.argumentsDone || c.item.ID != item.ID || c.item.CallID != item.CallID || c.item.Name != item.Name {
					return errors.New("completed tool disagrees with streamed call")
				}
				if c.arguments.String() != item.Arguments {
					return errConflictingToolArguments
				}
				c.finished = true
			} else if item.Type != "reasoning" && item.Type != "message" {
				return errors.New("unsupported Responses output item")
			}
			if event.OutputIndex == nil || *event.OutputIndex < 0 || nativeItems[*event.OutputIndex] != nil || item.Type != "function_call" && calls[*event.OutputIndex] != nil {
				return errors.New("invalid or duplicate completed Responses output index")
			}
			nativeItems[*event.OutputIndex] = append(json.RawMessage(nil), event.Item...)
			retainedBytes += len(event.Item)
			// Native items stay private until response.completed. In particular,
			// buffered reasoning alone must not prevent a safe transport retry.
		case "response.completed":
			indices := make([]int, 0, len(calls))
			for _, c := range calls {
				if !c.finished {
					return errors.New("response completed with unfinished tool arguments")
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
					return err
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
			return streamFailure(event)
		}
		if retainedBytes > 32<<20 {
			return errors.New("Responses output exceeds 32 MiB")
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
				return committed, errors.New("Responses stream event exceeds 8 MiB")
			}
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if e := scan.Err(); e != nil {
		if errors.Is(e, bufio.ErrTooLong) {
			return committed, errors.New("Responses stream line exceeds 8 MiB")
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

// streamFailure reports the documented terminal event fields. Keep backend
// recovery guidance readable, control-safe and bounded to 4096 UTF-8 bytes.
func streamFailure(event wireEvent) error {
	parts := []string{event.Type}
	if event.Type == "error" {
		parts = append(parts, event.Code, event.Message)
		if event.Param != "" {
			parts = append(parts, "parameter="+event.Param)
		}
	} else {
		if event.Response.Error != nil {
			parts = append(parts, event.Response.Error.Code, event.Response.Error.Message)
		}
		if event.Response.Incomplete != nil {
			parts = append(parts, "reason="+event.Response.Incomplete.Reason)
		}
	}
	text := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || r == 0x202e || r == 0x202d || r == 0x202a || r == 0x202b || r == 0x202c || r == 0x2066 || r == 0x2067 || r == 0x2068 || r == 0x2069 {
			return -1
		}
		return r
	}, strings.ToValidUTF8(strings.Join(parts, " "), "�"))
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 4096 {
		text = text[:4093]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		text += "…"
	}
	err := fmt.Errorf("subscription stream terminated: %s", text)
	code := event.Code
	if event.Response.Error != nil {
		code = event.Response.Error.Code
	}
	switch code {
	case "server_error", "rate_limit_exceeded", "temporarily_unavailable":
		return &llm.TransientError{Err: err}
	}
	return err
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
