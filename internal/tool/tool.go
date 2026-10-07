// Package tool provides strict, versioned tools and portable execution records.
package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/render"
)

// Error is an actionable, model-facing failure.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Fail constructs a structured execution or validation error.
func Fail(code, message string) *Error { return &Error{Code: code, Message: message} }

// Execution identifies the immutable call and its live actor/session.
type Execution struct {
	SessionID, CallID, Actor string
	Update                   func(any)            // Optional transient result update. Call serially; the runtime owns presentation and persistence.
	BinaryFiles              []llm.BinaryFileType // Binary capabilities frozen for the producing request; callers own an immutable copy.
}

// Output separates JSON metadata from native binary files. File references retain
// a checksum for verified request-time loading, rather than storing original bytes.
type Output struct {
	Value any
	Files []llm.BinaryFile
}

// Record holds exact arguments/result/presentation; decoding it never starts work.
type Record struct {
	Name      string           `json:"name"`
	Arguments json.RawMessage  `json:"arguments"`
	Result    json.RawMessage  `json:"result"`
	Markdown  render.Markdown  `json:"markdown"`
	Files     []llm.BinaryFile `json:"files,omitempty"` // Native binary files, separate from textual Result.
}

// Encode serializes the historical record at version one.
func (r Record) Encode() (int, []byte, error) { b, e := json.Marshal(r); return 1, b, e }

// Call is a validated serializable input with no live resources.
type Call interface {
	Encode() (int, []byte, error)
	Run(context.Context, Execution) (any, error)
}

// Tool owns strict codecs and its stable definition.
type Tool interface {
	Definition() llm.ToolDefinition
	DecodeCall(int, []byte) (Call, error)
	DecodeRecord(int, []byte) (Record, error)
}

// Strict rejects unknown fields, trailing values, non-object JSON, and oversized input.
func Strict(data []byte, v any) error {
	if len(data) > 16<<20 {
		return errors.New(prompts.ToolInputTooLarge)
	}
	trim := bytes.TrimSpace(data)
	if len(trim) == 0 || trim[0] != '{' {
		return errors.New(prompts.ToolExpectedJSONObject)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	if e := d.Decode(new(any)); e != io.EOF {
		return errors.New(prompts.ToolTrailingJSON)
	}
	return nil
}

type typed[A any] struct {
	def      llm.ToolDefinition
	validate func(A) error
	run      func(context.Context, Execution, A) (any, error)
}
type typedCall[A any] struct {
	args A
	run  func(context.Context, Execution, A) (any, error)
}

func (c typedCall[A]) Encode() (int, []byte, error) { b, e := json.Marshal(c.args); return 1, b, e }
func (c typedCall[A]) Run(ctx context.Context, x Execution) (any, error) {
	return c.run(ctx, x, c.args)
}
func (t typed[A]) Definition() llm.ToolDefinition { return t.def }
func (t typed[A]) DecodeCall(version int, b []byte) (Call, error) {
	if version != 1 {
		return nil, errors.New("unsupported tool call version")
	}
	var a A
	if e := Strict(b, &a); e != nil {
		return nil, e
	}
	if e := t.validate(a); e != nil {
		return nil, e
	}
	return typedCall[A]{a, t.run}, nil
}
func (t typed[A]) DecodeRecord(version int, b []byte) (Record, error) {
	var r Record
	if version != 1 {
		return r, errors.New("unsupported tool record version")
	}
	if e := Strict(b, &r); e != nil {
		return r, e
	}
	if r.Name != t.def.Name || !json.Valid(r.Arguments) || !json.Valid(r.Result) || r.Markdown.Revision != 1 {
		return r, errors.New("invalid historical tool record")
	}
	return r, nil
}

// Registry owns a stable, sorted tool catalog and the dispatcher.
type Registry struct {
	tools  map[string]Tool
	Detail func(Execution, []byte) (string, error)
}

// NewRegistry constructs an empty registry.
func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

// Register adds a typed tool; duplicate names are programmer errors.
func Register[A any](r *Registry, name, description string, properties map[string]any, required []string, validate func(A) error, run func(context.Context, Execution, A) (any, error)) {
	if _, ok := r.tools[name]; ok {
		panic("duplicate tool " + name)
	}
	if properties == nil {
		properties = map[string]any{}
	}
	if required == nil {
		required = []string{}
	}
	schema, _ := json.Marshal(map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false})
	r.tools[name] = typed[A]{llm.ToolDefinition{Name: name, Description: description, Parameters: schema}, validate, run}
}

// Property constructs a JSON Schema property, with optional enum values.
func Property(kind string, enum ...string) map[string]any {
	p := map[string]any{"type": kind}
	if len(enum) > 0 {
		p["enum"] = enum
	}
	return p
}

// Definitions returns sorted, stable schemas for cache reuse.
func (r *Registry) Definitions() []llm.ToolDefinition {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]llm.ToolDefinition, 0, len(names))
	for _, name := range names {
		out = append(out, r.tools[name].Definition())
	}
	return out
}

// Get returns a codec by exact tool name.
func (r *Registry) Get(name string) (Tool, bool) { t, ok := r.tools[name]; return t, ok }

// Filter copies a restricted catalog and dispatcher. Shared tool implementations
// keep their original ownership; excluded tools cannot be invoked through it.
func (r *Registry) Filter(allow func(string) bool) *Registry {
	out := NewRegistry()
	out.Detail = r.Detail
	for name, t := range r.tools {
		if allow(name) {
			out.tools[name] = t
		}
	}
	return out
}

// Invoke returns failures as serializable records; it does not persist or replay calls.
func (r *Registry) Invoke(ctx context.Context, x Execution, name string, args json.RawMessage) Record {
	var value any
	var err error
	t, ok := r.tools[name]
	if !ok {
		err = Fail("unknown_tool", fmt.Sprintf(prompts.ToolUnknown, name))
	} else {
		var call Call
		call, err = t.DecodeCall(1, args)
		if err != nil {
			err = Fail("invalid_input", err.Error())
		} else {
			value, err = call.Run(ctx, x)
		}
	}
	var file *fileResult
	var files []llm.BinaryFile
	if output, ok := value.(Output); ok {
		value, files = output.Value, output.Files
	}
	if err != nil {
		files = nil
	}
	if presented, ok := value.(fileResult); ok {
		file, value = &presented, presented.value
	}
	result := map[string]any{"ok": true}
	if err != nil {
		var te *Error
		if !errors.As(err, &te) {
			code := "execution_failed"
			if errors.Is(err, context.Canceled) {
				code = "cancelled"
			}
			if errors.Is(err, context.DeadlineExceeded) {
				code = "timeout"
			}
			if strings.Contains(err.Error(), "no such file") {
				code = "not_found"
			}
			te = Fail(code, err.Error())
		}
		result = map[string]any{"ok": false, "error": te}
	} else if value != nil {
		b, e := json.Marshal(value)
		if e == nil {
			e = json.Unmarshal(b, &result)
		}
		if e != nil {
			result = map[string]any{"ok": false, "error": Fail("execution_failed", e.Error())}
			files = nil
		}
	}
	b, e := json.Marshal(result)
	if e != nil {
		b, _ = json.Marshal(map[string]any{"ok": false, "error": Fail("execution_failed", prompts.ToolResultEncodingFailed)})
		files = nil
	}
	md := render.Tool(name, args, b)
	if file != nil {
		md.Summary += file.summary
		md.Detail = file.detail + md.Detail
	}
	if len(b) > 64<<10 {
		files = nil
		path := ""
		if r.Detail != nil {
			path, e = r.Detail(x, b)
		}
		if path == "" || e != nil {
			b, _ = json.Marshal(map[string]any{"ok": false, "error": Fail("result_too_large", prompts.ToolResultTooLarge)})
		} else {
			b, _ = json.Marshal(map[string]any{"ok": false, "truncated": true, "detail_path": path, "error": Fail("result_too_large", prompts.ToolResultRetained)})
		}
		md.Summary = render.Inline(name + prompts.ToolSummaryResultTooLarge)
	}
	return Record{Name: name, Arguments: args, Result: b, Markdown: md, Files: files}
}

// Required validates nonempty required text without changing whitespace semantics.
func Required(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf(prompts.ToolRequired, name)
	}
	return nil
}
