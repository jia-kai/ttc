// Package tool provides strict, versioned tools and portable execution records.
package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
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

// Call is a decoded serializable input with no live resources. It may be run only
// after DecodeCall succeeds; a validation failure can still return warning metadata.
type Call interface {
	Encode() (int, []byte, error)
	Run(context.Context, Execution) (any, error)
	// InputWarnings reports adjustments made while decoding, including on validation failure.
	InputWarnings() []InputWarning
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
	def       llm.ToolDefinition
	validate  func(A) error
	run       func(context.Context, Execution, A) (any, error)
	normalize func(*A) []InputWarning
	bounds    []integerBound
}
type typedCall[A any] struct {
	args     A
	run      func(context.Context, Execution, A) (any, error)
	warnings []InputWarning
}

// InputWarning describes a safe adjustment to a decoded argument. Bounds and
// message retain the numeric clipping contract; bounds apply only to clipping.
type InputWarning struct {
	Code      string `json:"code"`
	Parameter string `json:"parameter"`
	Requested any    `json:"requested"`
	Effective any    `json:"effective"`
	Minimum   int    `json:"minimum"`
	Maximum   int    `json:"maximum"`
	Message   string `json:"message"`
}

// MarshalJSON keeps clipping's numeric contract while omitting inapplicable
// bounds and empty explanatory text from normalization warnings.
func (w InputWarning) MarshalJSON() ([]byte, error) {
	if w.Code == "input_clipped" {
		type plain InputWarning
		return json.Marshal(plain(w))
	}
	return json.Marshal(struct {
		Code      string `json:"code"`
		Parameter string `json:"parameter"`
		Requested any    `json:"requested"`
		Effective any    `json:"effective"`
		Message   string `json:"message,omitempty"`
	}{w.Code, w.Parameter, w.Requested, w.Effective, w.Message})
}

// AdjustedInput reports a meaningful non-numeric adjustment. Values must be
// immutable JSON-serializable snapshots, not pointers to arguments being changed.
func AdjustedInput(parameter string, requested, effective any) []InputWarning {
	return []InputWarning{{Code: "input_normalized", Parameter: parameter, Requested: requested, Effective: effective}}
}

// NormalizeEnum trims and lowercases only recognized finite enum spellings.
// Unknown values are left unchanged for the caller's normal validation.
func NormalizeEnum(parameter string, value *string, allowed ...string) []InputWarning {
	canonical := strings.ToLower(strings.TrimSpace(*value))
	for _, candidate := range allowed {
		if canonical == candidate {
			if canonical == *value {
				return nil
			}
			warnings := AdjustedInput(parameter, *value, canonical)
			*value = canonical
			return warnings
		}
	}
	return nil
}

func (c typedCall[A]) InputWarnings() []InputWarning { return c.warnings }

// ClipInteger clips value to the inclusive bounds and reports any change.
// The caller must provide a non-nil value and minimum <= maximum.
// An unchanged value produces no warning.
func ClipInteger(parameter string, value *int, minimum, maximum int) []InputWarning {
	requested := *value
	*value = max(minimum, min(maximum, requested))
	if requested == *value {
		return nil
	}
	return []InputWarning{{
		Code: "input_clipped", Parameter: parameter, Requested: requested,
		Effective: *value, Minimum: minimum, Maximum: maximum,
		Message: fmt.Sprintf(prompts.ToolInputClipped, parameter, requested, *value, minimum, maximum),
	}}
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
	var warnings []InputWarning
	if len(t.bounds) > 0 {
		value := reflect.ValueOf(&a).Elem()
		for _, bound := range t.bounds {
			field := value.FieldByIndex(bound.index)
			if field.IsNil() {
				continue
			}
			field = field.Elem()
			effective := int(field.Int())
			warnings = append(warnings, ClipInteger(bound.name, &effective, bound.minimum, bound.maximum)...)
			field.SetInt(int64(effective))
		}
	}
	if t.normalize != nil {
		warnings = append(warnings, t.normalize(&a)...)
	}
	call := typedCall[A]{args: a, run: t.run, warnings: warnings}
	if e := t.validate(a); e != nil {
		return call, e
	}
	return call, nil
}

type integerBound struct {
	name             string
	minimum, maximum int
	index            []int
}

// integerBounds compiles schema bounds to top-level *int fields once.
// Unsupported bounded fields are registration errors, not input errors.
func integerBounds[A any](schema []byte) []integerBound {
	var definition struct {
		Properties map[string]struct {
			Type    string `json:"type"`
			Minimum *int   `json:"minimum"`
			Maximum *int   `json:"maximum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &definition); err != nil {
		panic(fmt.Sprintf("invalid tool schema: %v", err))
	}
	var bounds []integerBound
	for name, property := range definition.Properties {
		if property.Type == "integer" && property.Minimum != nil && property.Maximum != nil {
			if *property.Minimum > *property.Maximum {
				panic("invalid integer bounds for " + name)
			}
			argsType := reflect.TypeFor[A]()
			if argsType.Kind() != reflect.Struct {
				panic("integer bounds require struct arguments")
			}
			var index []int
			for i := 0; i < argsType.NumField(); i++ {
				field := argsType.Field(i)
				jsonName, _, _ := strings.Cut(field.Tag.Get("json"), ",")
				if jsonName == "-" {
					continue
				}
				if jsonName == "" {
					jsonName = field.Name
				}
				if jsonName != name {
					continue
				}
				if index != nil || !field.IsExported() || field.Anonymous || field.Type != reflect.TypeFor[*int]() {
					panic("unsupported integer bound field " + name)
				}
				index = field.Index
			}
			if index == nil {
				panic("missing top-level integer bound field " + name)
			}
			bounds = append(bounds, integerBound{name: name, minimum: *property.Minimum, maximum: *property.Maximum, index: index})
		}
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i].name < bounds[j].name })
	return bounds
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

// Register adds a typed tool; duplicate names are programmer errors. Optional
// normalizer runs after schema clipping and before the single validation pass.
// More than one normalizer or an unsupported bounded field is a programmer error.
func Register[A any](r *Registry, name, description string, properties map[string]any, required []string, validate func(A) error, run func(context.Context, Execution, A) (any, error), normalize ...func(*A) []InputWarning) {
	if _, ok := r.tools[name]; ok {
		panic("duplicate tool " + name)
	}
	if len(normalize) > 1 {
		panic("multiple tool input normalizers")
	}
	var normalizer func(*A) []InputWarning
	if len(normalize) == 1 {
		normalizer = normalize[0]
	}
	if properties == nil {
		properties = map[string]any{}
	}
	if required == nil {
		required = []string{}
	}
	schema, err := json.Marshal(map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false})
	if err != nil {
		panic(fmt.Sprintf("invalid tool schema: %v", err))
	}
	r.tools[name] = typed[A]{def: llm.ToolDefinition{Name: name, Description: description, Parameters: schema}, validate: validate, run: run, normalize: normalizer, bounds: integerBounds[A](schema)}
}

// Property constructs a JSON Schema property, with optional enum values.
func Property(kind string, enum ...string) map[string]any {
	p := map[string]any{"type": kind}
	if len(enum) > 0 {
		p["enum"] = enum
	}
	return p
}

// IntegerRange constructs an integer JSON Schema property with inclusive bounds.
// Decoded integer fields outside these bounds are clipped before validation.
// The corresponding argument field must be a top-level *int so omitted and null
// inputs remain unset rather than being clipped from a scalar zero value.
func IntegerRange(min, max int) map[string]any {
	return map[string]any{"type": "integer", "minimum": min, "maximum": max}
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
	var warnings []InputWarning
	t, ok := r.tools[name]
	if !ok {
		err = Fail("unknown_tool", fmt.Sprintf(prompts.ToolUnknown, name))
	} else {
		var call Call
		call, err = t.DecodeCall(1, args)
		if call != nil {
			warnings = call.InputWarnings()
		}
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
	marshalResult := func(result map[string]any) ([]byte, error) {
		if len(warnings) > 0 {
			if result == nil {
				result = map[string]any{}
			}
			result["warnings"] = warnings
		}
		return json.Marshal(result)
	}
	b, e := marshalResult(result)
	if e != nil {
		b, _ = marshalResult(map[string]any{"ok": false, "error": Fail("execution_failed", prompts.ToolResultEncodingFailed)})
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
			b, _ = marshalResult(map[string]any{"ok": false, "error": Fail("result_too_large", prompts.ToolResultTooLarge)})
		} else {
			b, _ = marshalResult(map[string]any{"ok": false, "truncated": true, "detail_path": path, "error": Fail("result_too_large", prompts.ToolResultRetained)})
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
