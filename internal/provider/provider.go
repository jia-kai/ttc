// Package provider defines frontend-independent model, login, and stream contracts.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
)

// Budget describes planned token reserves, including reasoning output.
// MaxOutputTokens is zero when the endpoint does not publish an output ceiling;
// OutputAllowance then reserves context without promising a transport-enforced cap.
type Budget struct {
	ContextLimit           int `json:"context_limit"`
	MaxOutputTokens        int `json:"max_output_tokens"`
	OutputAllowance        int `json:"output_allowance"`
	EstimationMargin       int `json:"estimation_margin"`
	RecentTokensMin        int `json:"recent_tokens_min"` // Soft minimum for the recent model/tool tail, excluding independently retained human inputs.
	RecentTokensMax        int `json:"recent_tokens_max"` // Hard maximum for that tail; oversized complete cycles are summarized.
	NextTurnInputReserve   int `json:"next_turn_input_reserve"`
	SummaryOutputAllowance int `json:"summary_output_allowance"`
}

// Validate rejects budgets that cannot leave room for instructions and output.
func (b Budget) Validate() error {
	if b.ContextLimit <= 0 || b.MaxOutputTokens < 0 || b.OutputAllowance <= 0 || b.SummaryOutputAllowance <= 0 || b.EstimationMargin < 0 || b.RecentTokensMin < 0 || b.RecentTokensMax <= 0 || b.RecentTokensMin > b.RecentTokensMax || b.NextTurnInputReserve < 0 {
		return fmt.Errorf("invalid model token budget")
	}
	if (b.MaxOutputTokens > 0 && (b.OutputAllowance > b.MaxOutputTokens || b.SummaryOutputAllowance > b.MaxOutputTokens)) || b.OutputAllowance+b.EstimationMargin+b.NextTurnInputReserve+b.SummaryOutputAllowance >= b.ContextLimit {
		return fmt.Errorf("model reserves exceed capacity")
	}
	return nil
}

// ModelSpec is provider-supplied metadata; Variants contains valid reasoning presets.
type ModelSpec struct {
	ID                  string            `json:"id"`
	BaseID              string            `json:"base_id,omitempty"`      // Catalog model ID when this picker choice has a distinct ID.
	ServiceTier         string            `json:"service_tier,omitempty"` // Explicit provider tier; empty means ordinary processing.
	Description         string            `json:"description,omitempty"`
	Name                string            `json:"name"`
	Variants            []string          `json:"variants"`
	VariantDescriptions map[string]string `json:"variant_descriptions,omitempty"` // Provider explanations keyed by variant ID.
	DefaultVariant      string            `json:"default_variant"`
	Images              bool              `json:"images"`
	SupportsReasoning   bool              `json:"supports_reasoning"` // Variants are explicit reasoning efforts, including "none" when listed.
	Budget              Budget            `json:"budget"`
	Revision            string            `json:"revision"`
}

// RequestID returns the underlying provider model, independent of speed choices.
func (m ModelSpec) RequestID() string {
	if m.BaseID != "" {
		return m.BaseID
	}
	return m.ID
}

// Selection freezes a model and its options for one request and its tool batch.
type Selection struct {
	Provider string    `json:"provider"`
	Model    ModelSpec `json:"model"`
	Variant  string    `json:"variant"`
}

// Resolve validates a selection without silently substituting another model.
func Resolve(provider string, models []ModelSpec, id, variant string) (Selection, error) {
	for _, m := range models {
		if m.ID != id {
			continue
		}
		if err := m.Budget.Validate(); err != nil {
			return Selection{}, err
		}
		if variant == "" {
			variant = m.DefaultVariant
		}
		for _, v := range m.Variants {
			if v == variant {
				return Selection{provider, m, variant}, nil
			}
		}
		return Selection{}, fmt.Errorf("unknown variant %q for %s", variant, id)
	}
	return Selection{}, fmt.Errorf("unknown model %q", id)
}

// ToolDefinition exposes a JSON object schema to the provider.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall is a complete provider call, emitted only after its arguments are complete.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolStart identifies an announced call whose streamed arguments are incomplete.
// It is display metadata only and must never be passed to a tool executor.
type ToolStart struct {
	ID   string `json:"call_id"`
	Name string `json:"name"`
}

// Image is either a queued human attachment's data URL or a file-backed tool
// image. Tool images persist only an absolute Path and SHA256. Original encoded
// bytes live in a disposable filesystem cache; a cache miss verifies the source
// without resizing, recompression or substituting changed contents.
type Image struct {
	Path    string `json:"path"`
	DataURL string `json:"data_url,omitempty"`
	SHA256  string `json:"sha256,omitempty"` // Lowercase hex SHA-256 of original encoded bytes; mutually exclusive with DataURL.
}

// ReplayState is an immutable, versioned adapter-owned response payload. Model
// is the provider's underlying model ID, independent of UI speed/variant choices.
// Adapters validate their own codec and never replay another provider/model's data.
// Items replace the entire canonical message on the wire, rather than augmenting it.
type ReplayState struct {
	Provider        string            `json:"provider"`
	Model           string            `json:"model"`
	Version         int               `json:"version"`
	Items           []json.RawMessage `json:"items"`
	EstimatedTokens *int              `json:"-"` // Adapter-derived context occupancy; transient, never wire/history metadata.
}

// ReplayTokens estimates opaque transport data when an adapter has not supplied
// a model-visible estimate. It is conservative, not endpoint-reported usage.
func ReplayTokens(state *ReplayState) int {
	if state == nil {
		return 0
	}
	if state.EstimatedTokens != nil {
		return max(0, *state.EstimatedTokens)
	}
	n := 0
	for _, item := range state.Items {
		n += (len(item) + 2) / 3
	}
	return n
}

// Message is canonical history. State preserves provider-native replay data;
// Content and Calls remain usable when a different provider/model is selected.
type Message struct {
	EventSeq    int64        `json:"event_seq,omitempty"`     // Source identity of an injected runtime event, never a provider wire field.
	RequestID   int64        `json:"request_id,omitempty"`    // Durable producing request for audit/inspection.
	Runtime     bool         `json:"runtime,omitempty"`       // Injected job/timer notices, rather than human instructions.
	InputSource string       `json:"input_source,omitempty"`  // Human input source: normal (empty defaults to normal), queue, steer, task or btw.
	InputTimeMS int64        `json:"input_time_ms,omitempty"` // Original human-input commit time, in Unix milliseconds; never refreshed by compaction.
	Role        string       `json:"role"`
	Phase       string       `json:"phase,omitempty"` // Adapter-supplied assistant phase, retained if native state is compacted.
	Content     string       `json:"content,omitempty"`
	UserText    *string      `json:"user_text,omitempty"` // Authored human text before attachment expansion; nil uses Content for display.
	Calls       []ToolCall   `json:"calls,omitempty"`
	CallID      string       `json:"call_id,omitempty"`
	Images      []Image      `json:"images,omitempty"`
	State       *ReplayState `json:"state,omitempty"`
}

// DisplayText returns authored human text when attachment expansion is present.
// Providers and inspectors use Content, which preserves the complete input.
func (m Message) DisplayText() string {
	if m.Role == "user" && !m.Runtime && m.UserText != nil {
		return *m.UserText
	}
	return m.Content
}

// AppendState takes ownership of a copied adapter item and enforces one codec
// per response. State items are never executable tool calls on their own.
func (m *Message) AppendState(selection Selection, version int, item json.RawMessage) error {
	if selection.Provider == "" || selection.Model.RequestID() == "" || version < 1 || !json.Valid(item) {
		return fmt.Errorf("invalid provider replay state")
	}
	if m.State == nil {
		m.State = &ReplayState{Provider: selection.Provider, Model: selection.Model.RequestID(), Version: version}
	}
	if m.State.Provider != selection.Provider || m.State.Model != selection.Model.RequestID() || m.State.Version != version {
		return fmt.Errorf("mixed provider replay state")
	}
	m.State.Items = append(m.State.Items, append(json.RawMessage(nil), item...))
	return nil
}

// ContextFor copies canonical messages and excludes replay state bound to another
// provider/model. Stored history stays unchanged; the adapter validates its codec.
func ContextFor(selection Selection, messages []Message) []Message {
	out := append([]Message(nil), messages...)
	for i, m := range out {
		if m.State != nil && (m.State.Provider != selection.Provider || m.State.Model != selection.Model.RequestID()) {
			out[i].State = nil
		}
	}
	return out
}

// Request contains immutable instructions, selection and canonical context,
// including developer messages. Providers own transport/state codecs and map
// roles to their API. NoTools prohibits executable calls, regardless of schemas.
type Request struct {
	ConversationID string // Opaque, stable per conversation/actor; providers may use it for cache affinity.
	Selection      Selection
	System         string
	Messages       []Message
	Tools          []ToolDefinition
	NoTools        bool
	OutputTokens   int
	MaxAttempts    int // Zero retries until cancellation; positive values bound total attempts.
}

// Usage reports endpoint input/output tokens; output includes reasoning when reported.
type Usage struct {
	InputTokens           int  `json:"input_tokens"`
	OutputTokens          int  `json:"output_tokens"`
	CachedInputTokens     *int `json:"cached_input_tokens,omitempty"`     // Subset of input; nil if unavailable.
	CacheWriteTokens      *int `json:"cache_write_tokens,omitempty"`      // Input written to cache, separate from cache reads; nil if unavailable.
	ReasoningOutputTokens *int `json:"reasoning_output_tokens,omitempty"` // Subset of output; nil if unavailable.
}

// Retry describes a pending retry of the same immutable request, before its wait.
// Reason is a safe explanation without credentials or raw transport errors.
type Retry struct {
	Attempt           int    `json:"attempt"`      // Next attempt, numbered from one.
	MaxAttempts       int    `json:"max_attempts"` // Zero means unlimited.
	DelayMilliseconds int64  `json:"delay_ms"`
	Reason            string `json:"reason"`
}

// StreamEvent is a delta, call announcement, complete call, adapter state, completion,
// or retry notice. A call_start never authorizes execution.
type StreamEvent struct {
	Kind         string
	Text         string
	Call         *ToolCall
	CallStart    *ToolStart      // Present for Kind "call_start", before arguments are complete.
	StateItem    json.RawMessage // Present for Kind "state"; persisted but never executed.
	StateVersion int             // Adapter's replay codec version for StateItem.
	Phase        string          // Present for Kind "phase"; adapter maps native assistant phase.
	Usage        *Usage
	ResponseID   string
	ServiceTier  string // Actual processing tier when the endpoint reports it.
	Retry        *Retry // Present for Kind "retry"; does not commit response output.
}

// LoginStep describes a device-code prompt or status, never conversation content.
type LoginStep struct {
	Kind           string
	URL            string
	Code           string
	Message        string
	ExpiresSeconds int
}

// LoginAnswer is reserved for flows that require a typed user response.
type LoginAnswer struct{ Text string }

// LoginUI renders provider-owned steps without coupling the provider to widgets.
type LoginUI interface {
	Present(context.Context, LoginStep) (LoginAnswer, error)
}

// Provider supplies model metadata and cancellable streams. Emit errors stop a stream.
type Provider interface {
	Models(context.Context) ([]ModelSpec, error)
	Login(context.Context, LoginUI) error
	Stream(context.Context, Request, func(StreamEvent) error) error
	// EstimateReplay returns nonnegative estimated context tokens for one native
	// assistant message. It never changes replay bytes or endpoint usage counters.
	EstimateReplay(Message) int
}
