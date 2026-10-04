package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"ttc/internal/provider"
	"ttc/internal/render"
)

// catalogClientVersion negotiates the verified Codex catalog contract, not TTC's release version.
// The backend excludes ordinary models for older versions even when authentication succeeds.
const catalogClientVersion = "0.159.0"

const catalogMaxAttempts = 3
const catalogTimeout = 30 * time.Second

// Models obtains subscription catalog metadata without consuming inference tokens.
// Discovery has a 30-second overall timeout and at most three HTTP attempts for
// transport errors, HTTP 429 or 5xx. Authentication and catalog errors are final.
func (a *Adapter) Models(ctx context.Context) ([]provider.ModelSpec, error) {
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	tokens, e := a.auth(ctx)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, "GET", a.BaseURL+"/models?client_version="+catalogClientVersion, nil)
	if e != nil {
		return nil, e
	}
	if (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Host == "" {
		return nil, errors.New("subscription endpoint requires an HTTP(S) URL with a host")
	}
	headers(req, tokens)
	var resp *http.Response
	for attempt := 0; attempt < catalogMaxAttempts; attempt++ {
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		resp, e = a.Client.Do(req.Clone(ctx))
		header := ""
		if e != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if invalidCertificate(e) {
				return nil, errors.New("subscription TLS certificate verification failed")
			}
			e = fmt.Errorf("model catalog transport failed (attempt %d/%d): %w", attempt+1, catalogMaxAttempts, e)
		} else if resp.StatusCode == http.StatusOK {
			break
		} else {
			status := resp.StatusCode
			header = resp.Header.Get("Retry-After")
			resp.Body.Close()
			e = fmt.Errorf("model catalog HTTP %d (attempt %d/%d)", status, attempt+1, catalogMaxAttempts)
			if status != 429 && (status < 500 || status > 599) {
				return nil, e
			}
		}
		if attempt == catalogMaxAttempts-1 {
			return nil, e
		}
		if e = wait(ctx, retryDelay(attempt, header, time.Now())); e != nil {
			return nil, e
		}
	}
	defer resp.Body.Close()
	var catalog struct {
		Models []struct {
			Slug       string `json:"slug"`
			Name       string `json:"display_name"`
			Default    string `json:"default_reasoning_level"`
			Visibility string `json:"visibility"`
			Priority   int    `json:"priority"`
			Levels     []struct {
				Effort      string `json:"effort"`
				Description string `json:"description"`
			} `json:"supported_reasoning_levels"`
			Context    int                                      `json:"context_window"`
			MaxContext int                                      `json:"max_context_window"`
			Effective  int                                      `json:"effective_context_window_percent"`
			Modalities []string                                 `json:"input_modalities"`
			Tiers      []struct{ ID, Name, Description string } `json:"service_tiers"`
		} `json:"models"`
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&catalog); e != nil {
		return nil, e
	}
	sort.SliceStable(catalog.Models, func(i, j int) bool { return catalog.Models[i].Priority < catalog.Models[j].Priority })
	out := []provider.ModelSpec{}
	for _, m := range catalog.Models {
		if m.Visibility == "hide" {
			continue
		}
		if m.Visibility != "list" {
			return nil, fmt.Errorf("model %q has invalid catalog visibility %q", m.Slug, m.Visibility)
		}
		capacity := m.Context
		if capacity == 0 {
			capacity = m.MaxContext
		}
		if m.Effective > 0 && m.Effective <= 100 {
			capacity = capacity * m.Effective / 100
		}
		if capacity <= 0 {
			continue
		}
		variants := []string{}
		descriptions := map[string]string{}
		for _, v := range m.Levels {
			variants = append(variants, v.Effort)
			descriptions[v.Effort] = v.Description
		}
		if len(variants) == 0 {
			variants = []string{"none"}
			m.Default = "none"
		}
		images := false
		for _, mode := range m.Modalities {
			images = images || mode == "image"
		}
		budget := provider.Budget{ContextLimit: capacity, OutputAllowance: 4096, EstimationMargin: 4096, RecentTokensMin: 4096, RecentTokensMax: 16000, NextTurnInputReserve: 4096, SummaryOutputAllowance: 2048}
		if e = budget.Validate(); e != nil {
			continue
		}
		model := provider.ModelSpec{ID: m.Slug, Name: m.Name, Variants: variants, VariantDescriptions: descriptions, DefaultVariant: m.Default, Images: images, SupportsReasoning: len(m.Levels) > 0, Budget: budget, Revision: resp.Header.Get("ETag")}
		if _, e := provider.Resolve("openai", []provider.ModelSpec{model}, model.ID, ""); e != nil {
			return nil, fmt.Errorf("invalid subscription model metadata: %w", e)
		}
		out = append(out, model)
		for _, tier := range m.Tiers {
			if tier.Name != "Fast" {
				continue
			}
			if tier.ID != "priority" && tier.ID != "fast" {
				return nil, fmt.Errorf("model %q has unsupported Fast tier %q", m.Slug, tier.ID)
			}
			out[len(out)-1].Name = m.Name + " (Standard)"
			fast := model
			fast.ID, fast.BaseID = m.Slug+"/fast", m.Slug
			fast.Name, fast.ServiceTier, fast.Description = m.Name+" (Fast)", tier.ID, tier.Description
			out = append(out, fast)
		}
	}
	seen := map[string]bool{}
	for _, m := range out {
		if m.ID == "" || seen[m.ID] {
			return nil, fmt.Errorf("empty or duplicate catalog choice %q", m.ID)
		}
		seen[m.ID] = true
	}
	if len(out) == 0 {
		return nil, errors.New("subscription catalog contains no selectable models; hidden entries and invalid context budgets are excluded")
	}
	return out, nil
}
func headers(req *http.Request, t Tokens) {
	req.Header.Set("Authorization", "Bearer "+t.Access)
	req.Header.Set("ChatGPT-Account-ID", t.AccountID)
	req.Header.Set("originator", "ttc")
	req.Header.Set("User-Agent", "ttc/0.1")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
}

// imagePart degrades only unavailable original bytes, not invalid references,
// cancellation or cache failures. Canonical messages remain unchanged.
func imagePart(ctx context.Context, im provider.Image, textKind string) (map[string]any, error) {
	url, err := im.URL(ctx)
	if err != nil {
		var unavailable *provider.UnavailableImageError
		if errors.As(err, &unavailable) {
			return map[string]any{"type": textKind, "text": unavailable.Error()}, nil
		}
		return nil, err
	}
	return map[string]any{"type": "input_image", "image_url": url}, nil
}

func wire(ctx context.Context, req provider.Request) ([]byte, error) {
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
	for _, m := range provider.ContextFor(req.Selection, req.Messages) {
		if len(m.Images) > 0 && !req.Selection.Model.Images {
			return nil, errors.New("model does not support images")
		}
		if m.State != nil {
			if len(m.Images) > 0 {
				return nil, errors.New("OpenAI replay state does not support canonical images")
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
			if len(m.Images) > 0 {
				parts := []any{map[string]any{"type": "input_text", "text": m.Content}}
				for _, im := range m.Images {
					part, err := imagePart(ctx, im, "input_text")
					if err != nil {
						return nil, fmt.Errorf("tool output %q image: %w", m.CallID, err)
					}
					parts = append(parts, part)
				}
				output = parts
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.CallID, "output": output})
			continue
		}
		if m.Content != "" || len(m.Images) > 0 {
			content := []any{}
			kind := "input_text"
			if m.Role == "assistant" {
				kind = "output_text"
			}
			if m.Content != "" {
				content = append(content, map[string]any{"type": kind, "text": m.Content})
			}
			for _, im := range m.Images {
				part, err := imagePart(ctx, im, kind)
				if err != nil {
					return nil, fmt.Errorf("%s message image: %w", m.Role, err)
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
// The subscription endpoint has no verified output cap; OutputTokens reserves context only.
func (a *Adapter) Stream(ctx context.Context, req provider.Request, emit func(provider.StreamEvent) error) error {
	body, e := wire(ctx, req)
	if e != nil {
		return e
	}
	maxAttempts := req.MaxAttempts
	if maxAttempts < 0 {
		return errors.New("max attempts must be nonnegative")
	}
	for attempt := 0; maxAttempts == 0 || attempt < maxAttempts; attempt++ {
		tokens, e := a.auth(ctx)
		if e != nil {
			var transient *provider.TransientError
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
				return &provider.TransientError{Err: errors.New("subscription transport failed before response")}
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
				return &provider.TransientError{Err: err}
			}
			return err
		}
		callbackFailed := false
		committed, err := parseStream(resp.Body, func(event provider.StreamEvent) error {
			if req.NoTools && (event.Kind == "call" || event.Kind == "call_start") {
				return errors.New("OpenAI returned a tool to a no-tools request")
			}
			err := emit(event)
			callbackFailed = callbackFailed || err != nil
			return err
		})
		resp.Body.Close()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		reason := "temporary stream failure before output"
		if errors.Is(err, errStreamLost) && !callbackFailed {
			err = &provider.TransientError{Err: err}
			reason = "stream interrupted before output"
		}
		var transient *provider.TransientError
		if callbackFailed || committed || maxAttempts > 0 && attempt == maxAttempts-1 || !errors.As(err, &transient) {
			return err
		}
		if e = retryWait(ctx, emit, attempt, maxAttempts, reason, ""); e != nil {
			return e
		}
	}
	return errors.New("request attempts exhausted")
}

var errStreamLost = errors.New("Responses stream interrupted before completion")

func parseStream(reader io.Reader, emit func(provider.StreamEvent) error) (bool, error) {
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
		var out *provider.StreamEvent
		switch event.Type {
		case "response.output_text.delta", "response.refusal.delta":
			retainedBytes += len(event.Delta)
			streamedText.WriteString(event.Delta)
			out = &provider.StreamEvent{Kind: "text", Text: event.Delta}
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
				out = &provider.StreamEvent{Kind: "call_start", CallStart: &provider.ToolStart{ID: item.CallID, Name: item.Name}}
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
				retainedBytes += len(event.Delta)
			} else {
				if event.Arguments != c.arguments.String() || !validArguments(event.Arguments) {
					return errors.New("streamed tool arguments disagree with completed arguments")
				}
				c.argumentsDone = true
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
				if c == nil || c.finished || !c.argumentsDone || c.item.ID != item.ID || c.item.CallID != item.CallID || c.item.Name != item.Name || c.arguments.String() != item.Arguments {
					return errors.New("completed tool disagrees with streamed call")
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
			committed = true
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
			reply := provider.Message{Role: "assistant", Content: streamedText.String(), State: &provider.ReplayState{Version: replayVersion}}
			for _, index := range indices {
				reply.State.Items = append(reply.State.Items, nativeItems[index])
				if c := calls[index]; c != nil {
					reply.Calls = append(reply.Calls, provider.ToolCall{ID: c.item.CallID, Name: c.item.Name, Arguments: json.RawMessage(c.arguments.String())})
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
					if err := emit(provider.StreamEvent{Kind: "phase", Phase: native.Phase}); err != nil {
						return err
					}
				}
				if err := emit(provider.StreamEvent{Kind: "state", StateVersion: replayVersion, StateItem: nativeItems[index]}); err != nil {
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
				if err := emit(provider.StreamEvent{Kind: "call", Call: &provider.ToolCall{ID: c.item.CallID, Name: c.item.Name, Arguments: json.RawMessage(c.arguments.String())}}); err != nil {
					return err
				}
			}
			complete = true
			out = &provider.StreamEvent{Kind: "completed", Usage: event.Response.Usage.normalized(), ResponseID: event.Response.ID, ServiceTier: event.Response.ServiceTier}
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
	text := strings.Join(strings.Fields(render.Clean(strings.Join(parts, " "))), " ")
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
		return &provider.TransientError{Err: err}
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
	argumentsDone, finished bool
}

func validArguments(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "{") && json.Valid([]byte(text))
}
