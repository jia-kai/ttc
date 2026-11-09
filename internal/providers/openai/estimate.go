package openai

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"ttc/internal/llm"
	"ttc/internal/prompts"
)

// Each entry has fixed-size hashes/counts only. The entry limit also bounds
// memory, independently of request length, attachment size and actor count.
const measuredInputLimit = 256

// inputEncodingNamespace changes when native projection or local accounting
// changes. Exact request model IDs are also part of the compatibility digest;
// no tokenizer-family aliases are inferred. Tier and effort control generation,
// not input encoding, so they do not partition measurements.
const inputEncodingNamespace = "ttc/openai/native-input/v2/accounting-1"

type inputKey struct {
	compatible [sha256.Size]byte
	exact      [sha256.Size]byte
}

type measuredInput struct {
	key   inputKey
	local int
	input int
}

type inputTokenCache struct {
	mu      sync.Mutex
	entries map[inputKey]*list.Element
	recent  list.List // Front is most recently used; values are measuredInput.
}

// preparedTokenInput is immutable, request-owned metadata. Only its complete
// final prefix can become measured; interior hashes are lookup candidates, not
// inferred observations. Cumulative local counts make suffix lookup linear in
// item count without rescanning payloads or retaining payloads in the cache.
type preparedTokenInput struct {
	compatible [sha256.Size]byte
	prefixes   []preparedTokenPrefix // Includes the empty native sequence/header.
}

type preparedTokenPrefix struct {
	exact [sha256.Size]byte
	local int
}

// preparedOpenAIInput owns the exact wire bytes measured by its checkpoint.
// Neither its payload nor its canonical binding can be mutated by callers.
type preparedOpenAIInput struct {
	body    []byte
	tokens  preparedTokenInput
	binding [sha256.Size]byte
}

func (*preparedOpenAIInput) Provider() string { return "openai" }

// requestBinding checks mutable canonical input without resolving attachments or
// repeating native projection. Retry bookkeeping and callbacks are not input.
func requestBinding(req llm.Request) ([sha256.Size]byte, error) {
	type bindingCall struct{ ID, Name, Arguments string }
	type bindingMessage struct {
		Role, Content, Phase, CallID string
		Calls                        []bindingCall
		Files                        []llm.BinaryFile
		State                        *llm.ReplayState
	}
	messages := make([]bindingMessage, 0, len(req.Messages))
	for _, message := range llm.ContextFor(req.Selection, req.Messages) {
		content, err := message.ModelContent()
		if err != nil {
			return [sha256.Size]byte{}, err
		}
		binding := bindingMessage{Role: message.Role(), Content: content, CallID: message.CallID, State: message.State}
		if len(message.Files) > 0 {
			binding.Files = message.Files
		}
		if message.Type == llm.TypeAssistant {
			binding.Phase = message.Phase
		}
		for _, call := range message.Calls {
			// Arguments are native strings, so preserve their whitespace. Tool
			// schemas and replay items instead use normalized JSON on the wire.
			binding.Calls = append(binding.Calls, bindingCall{call.ID, call.Name, string(call.Arguments)})
		}
		messages = append(messages, binding)
	}
	tools := make([]nativeTool, 0, len(req.Tools))
	for _, tool := range req.Tools {
		tools = append(tools, nativeTool{Type: "function", Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters})
	}
	tier := req.Selection.Model.ServiceTier
	if tier == "" {
		tier = "default"
	}
	var reasoning *nativeReasoning
	if variant := req.Selection.Variant; variant != "" && (variant != "none" || req.Selection.Model.SupportsReasoning) {
		reasoning = &nativeReasoning{Effort: variant, Summary: "auto"}
	}
	value := struct {
		Conversation, Provider, Model, Tier, System string
		Reasoning                                   *nativeReasoning
		Messages                                    []bindingMessage
		Tools                                       []nativeTool
		NoTools                                     bool
	}{req.ConversationID, req.Selection.Provider, req.Selection.Model.RequestID(), tier, req.System, reasoning, messages, tools, req.NoTools}
	encoded, err := json.Marshal(value)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf(prompts.OpenAIBindPreparedInput, err)
	}
	return sha256.Sum256(encoded), nil
}

func (a *Adapter) preparedInput(ctx context.Context, req llm.Request) (*preparedOpenAIInput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	binding, err := requestBinding(req)
	if err != nil {
		return nil, err
	}
	if req.Prepared != nil {
		// This assertion is the explicit provider boundary for an opaque handle.
		prepared, ok := req.Prepared.(*preparedOpenAIInput)
		if !ok || prepared == nil {
			return nil, errors.New(prompts.OpenAIForeignPreparedInput)
		}
		if binding != prepared.binding {
			return nil, errors.New(prompts.OpenAIChangedPreparedInput)
		}
		return prepared, nil
	}
	native, err := prepareWire(ctx, req, a.resolveBinary)
	if err != nil {
		return nil, err
	}
	tokens, err := prepareTokenInput(ctx, native)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(native)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &preparedOpenAIInput{body: body, tokens: tokens, binding: binding}, nil
}

func (p preparedTokenInput) checkpoint() llm.TokenCheckpoint {
	last := p.prefixes[len(p.prefixes)-1]
	return llm.TokenCheckpoint{Version: 2, LocalInput: last.local,
		Compatible: hex.EncodeToString(p.compatible[:]), Exact: hex.EncodeToString(last.exact[:])}
}

// EstimateInput uses Stream's native projection and the longest complete input
// previously measured with this exact configuration. Only the unmeasured suffix
// is locally estimated; divergent or shorter inputs never borrow signed deltas.
// It performs no network inference and never learns from an estimate alone.
func (a *Adapter) EstimateInput(ctx context.Context, req llm.Request) (llm.InputEstimate, error) {
	invalid := llm.InputEstimate{Input: -1, Estimated: true}
	prepared, err := a.preparedInput(ctx, req)
	if err != nil {
		return invalid, err
	}
	result, err := a.estimates.estimate(prepared.tokens)
	if err != nil {
		return invalid, err
	}
	if err := ctx.Err(); err != nil {
		return invalid, err
	}
	result.Prepared = prepared
	return result, nil
}

// RestoreEstimate restores only validated native version 2 records bound by the
// caller to completed usable usage. Legacy version 1 is a validated no-op: its
// hashes describe a different accounting policy and cannot seed this cache.
// Invalid input leaves cache state unchanged.
func (a *Adapter) RestoreEstimate(checkpoint llm.TokenCheckpoint, usage llm.Usage) error {
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	if checkpoint.Version == 1 {
		return nil
	}
	if !usage.Usable() {
		return errors.New(prompts.OpenAIRestoreEstimateUsage)
	}
	a.estimates.remember(checkpoint, usage.InputTokens)
	return nil
}

func (c *inputTokenCache) remember(checkpoint llm.TokenCheckpoint, input int) {
	var key inputKey
	compatible, _ := hex.DecodeString(checkpoint.Compatible)
	exact, _ := hex.DecodeString(checkpoint.Exact)
	copy(key.compatible[:], compatible)
	copy(key.exact[:], exact)
	entry := measuredInput{key: key, local: checkpoint.LocalInput, input: input}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[inputKey]*list.Element)
	}
	if existing := c.entries[key]; existing != nil {
		existing.Value = entry
		c.recent.MoveToFront(existing)
		return
	}
	if len(c.entries) == measuredInputLimit {
		oldest := c.recent.Back()
		delete(c.entries, oldest.Value.(measuredInput).key)
		c.recent.Remove(oldest)
	}
	c.entries[key] = c.recent.PushFront(entry)
}

func (c *inputTokenCache) estimate(prepared preparedTokenInput) (llm.InputEstimate, error) {
	checkpoint := prepared.checkpoint()
	result := llm.InputEstimate{Input: checkpoint.LocalInput, Estimated: true, Checkpoint: &checkpoint}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(prepared.prefixes) - 1; i >= 0; i-- {
		prefix := prepared.prefixes[i]
		element := c.entries[inputKey{compatible: prepared.compatible, exact: prefix.exact}]
		if element == nil {
			continue
		}
		entry := element.Value.(measuredInput)
		// Counts must agree under this encoding namespace, including restored
		// records. A mismatched record cannot calibrate a native suffix.
		if entry.local != prefix.local {
			continue
		}
		suffix := checkpoint.LocalInput - prefix.local
		if entry.input > int(^uint(0)>>1)-suffix {
			return llm.InputEstimate{Input: -1, Estimated: true}, errors.New(prompts.OpenAIMeasuredTokenOverflow)
		}
		result.Input = entry.input + suffix
		result.Estimated = i != len(prepared.prefixes)-1
		c.recent.MoveToFront(element)
		break
	}
	return result, nil
}

func prepareTokenInput(ctx context.Context, native *nativeRequest) (preparedTokenInput, error) {
	if err := ctx.Err(); err != nil {
		return preparedTokenInput{}, err
	}
	// Include only input encoding configuration. Tier, reasoning effort, tool
	// choice and transport/cache affinity flags do not change input occupancy.
	header := struct {
		Namespace    string       `json:"encoding_namespace"`
		Model        string       `json:"model"`
		Instructions string       `json:"instructions"`
		Tools        []nativeTool `json:"tools"`
	}{inputEncodingNamespace, native.Model, native.Instructions, native.Tools}
	encoded, err := json.Marshal(header)
	if err != nil {
		return preparedTokenInput{}, fmt.Errorf(prompts.OpenAIPrepareInputConfiguration, err)
	}
	prepared := preparedTokenInput{compatible: sha256.Sum256(encoded)}
	local := textTokens(native.Instructions)
	for _, tool := range native.Tools {
		parameters, err := json.Marshal(tool.Parameters)
		if err != nil {
			return preparedTokenInput{}, err
		}
		count, err := tokenSum(16, textTokens(tool.Name), textTokens(tool.Description), textTokens(string(parameters)))
		if err != nil {
			return preparedTokenInput{}, err
		}
		local, err = tokenSum(local, count)
		if err != nil {
			return preparedTokenInput{}, err
		}
	}
	chain := prepared.compatible
	input := native.Input
	prepared.prefixes = make([]preparedTokenPrefix, 1, len(input)+1)
	prepared.prefixes[0] = preparedTokenPrefix{exact: chain, local: local}
	for _, item := range input {
		if err := ctx.Err(); err != nil {
			return preparedTokenInput{}, err
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return preparedTokenInput{}, fmt.Errorf(prompts.OpenAIPrepareInputItem, err)
		}
		itemHash := sha256.Sum256(encoded)
		var link [sha256.Size * 2]byte
		copy(link[:], chain[:])
		copy(link[sha256.Size:], itemHash[:])
		chain = sha256.Sum256(link[:])
		count, err := nativeItemTokens(item)
		if err != nil {
			return preparedTokenInput{}, err
		}
		local, err = tokenSum(local, count)
		if err != nil {
			return preparedTokenInput{}, err
		}
		prepared.prefixes = append(prepared.prefixes, preparedTokenPrefix{exact: chain, local: local})
	}
	return prepared, nil
}

func textTokens(text string) int { return len(text)/3 + (len(text)%3+2)/3 }

func tokenSum(counts ...int) (int, error) {
	n := 0
	for _, count := range counts {
		if count < 0 || count > int(^uint(0)>>1)-n {
			return 0, errors.New(prompts.LLMInputTokenOverflow)
		}
		n += count
	}
	return n, nil
}

// nativeItemTokens counts semantic occupancy, not JSON envelopes, identifiers or
// base64 expansion. Encrypted reasoning removes base64 expansion and the Codex
// encryption overhead before applying its coarse four-bytes-per-token heuristic.
func nativeItemTokens(item map[string]any) (int, error) {
	text := func(key string) string { value, _ := item[key].(string); return value }
	switch text("type") {
	case "reasoning":
		if encrypted := text("encrypted_content"); encrypted != "" {
			decoded := len(encrypted)/4*3 + len(encrypted)%4*3/4
			return (max(0, decoded-650) + 3) / 4, nil
		}
		return nativePartsTokens(item["summary"])
	case "message":
		parts, err := nativePartsTokens(item["content"])
		if err != nil {
			return 0, err
		}
		return tokenSum(16, parts)
	case "function_call":
		return tokenSum(16, textTokens(text("name")), textTokens(text("arguments")))
	case "function_call_output":
		if output, ok := item["output"].(string); ok {
			return tokenSum(16, textTokens(output))
		}
		parts, err := nativePartsTokens(item["output"])
		if err != nil {
			return 0, err
		}
		return tokenSum(16, parts)
	default:
		return 0, errors.New(prompts.OpenAIUnsupportedEstimateItem)
	}
}

func nativePartsTokens(value any) (int, error) {
	parts, _ := value.([]any)
	n := 0
	for _, value := range parts {
		part, ok := value.(map[string]any)
		if !ok {
			return 0, errors.New(prompts.OpenAIInvalidEstimateContent)
		}
		count := 0
		for _, key := range []string{"text", "refusal"} {
			text, _ := part[key].(string)
			var err error
			count, err = tokenSum(count, textTokens(text))
			if err != nil {
				return 0, err
			}
		}
		for _, key := range []string{"image_url", "file_data"} {
			url, ok := part[key].(string)
			if !ok {
				continue
			}
			metadata, data, ok := strings.Cut(url, ";base64,")
			if !ok {
				return 0, errors.New(prompts.OpenAIInvalidEstimateBinary)
			}
			size := len(data)/4*3 + len(data)%4*3/4
			size -= len(data) - len(strings.TrimRight(data, "="))
			file := llm.BinaryFile{MIMEType: strings.TrimPrefix(metadata, "data:"), Bytes: size}
			var err error
			count, err = tokenSum(count, file.EstimatedTokens())
			if err != nil {
				return 0, err
			}
		}
		var err error
		n, err = tokenSum(n, count)
		if err != nil {
			return 0, err
		}
	}
	return n, nil
}
