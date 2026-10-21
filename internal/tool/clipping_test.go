package tool

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/llm"
)

type clippingArgs struct {
	First  *int   `json:"first,omitempty"`
	Second *int   `json:"second,omitempty"`
	Exact  int64  `json:"exact,omitempty"`
	Text   string `json:"text,omitempty"`
}

type clippingResult struct {
	OK       bool           `json:"ok"`
	Error    *Error         `json:"error"`
	Warnings []InputWarning `json:"warnings"`
}

func decodeClippingResult(t *testing.T, record Record) clippingResult {
	t.Helper()
	var result clippingResult
	if err := json.Unmarshal(record.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestIntegerRangeClipping(t *testing.T) {
	r := NewRegistry()
	validations, executions := 0, 0
	var received clippingArgs
	Register(r, "bounded", "", map[string]any{
		"first": IntegerRange(1, 10), "second": IntegerRange(-5, 5),
		"exact": Property("integer"), "text": Property("string"),
	}, nil, func(a clippingArgs) error {
		validations++
		if a.First == nil || *a.First != 1 || a.Second == nil || *a.Second != 5 {
			t.Fatalf("validation saw unnormalized args: %+v", a)
		}
		return nil
	}, func(_ context.Context, _ Execution, a clippingArgs) (any, error) {
		executions++
		received = a
		return map[string]any{"ok": true}, nil
	})
	definition := r.Definitions()[0]
	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if p := schema.Properties["first"]; p["type"] != "integer" || p["minimum"] != float64(1) || p["maximum"] != float64(10) {
		t.Fatalf("schema = %s", definition.Parameters)
	}
	original := json.RawMessage(`{ "first": -100, "second": 99, "exact": 9007199254740993 }`)
	record := r.Invoke(context.Background(), Execution{}, "bounded", original)
	result := decodeClippingResult(t, record)
	if !result.OK || executions != 1 || validations != 1 || received.Exact != 9007199254740993 {
		t.Fatalf("result=%s executions=%d validations=%d received=%+v", record.Result, executions, validations, received)
	}
	if string(record.Arguments) != string(original) {
		t.Fatalf("original arguments changed: %s", record.Arguments)
	}
	want := []InputWarning{
		{Code: "input_clipped", Parameter: "first", Requested: float64(-100), Effective: float64(1), Minimum: 1, Maximum: 10, Message: "first was clipped from -100 to 1 (accepted range 1-10)."},
		{Code: "input_clipped", Parameter: "second", Requested: float64(99), Effective: float64(5), Minimum: -5, Maximum: 5, Message: "second was clipped from 99 to 5 (accepted range -5-5)."},
	}
	if !reflect.DeepEqual(result.Warnings, want) {
		t.Fatalf("warnings = %+v", result.Warnings)
	}
	codec, _ := r.Get("bounded")
	call, err := codec.DecodeCall(1, original)
	if err != nil {
		t.Fatal(err)
	}
	version, encoded, err := call.Encode()
	var effective clippingArgs
	if err != nil || version != 1 || json.Unmarshal(encoded, &effective) != nil || !reflect.DeepEqual(effective, received) {
		t.Fatalf("effective encoding = %s, %v", encoded, err)
	}
	if executions != 1 {
		t.Fatal("decoding or encoding executed the tool")
	}
}

func TestClippingErrorsAndOmission(t *testing.T) {
	for _, mode := range []string{"validation", "execution", "success"} {
		t.Run(mode, func(t *testing.T) {
			r := NewRegistry()
			validations, executions := 0, 0
			Register(r, "bounded", "", map[string]any{"first": IntegerRange(1, 10)}, nil, func(a clippingArgs) error {
				validations++
				if mode == "validation" {
					return errors.New("semantic failure")
				}
				return nil
			}, func(context.Context, Execution, clippingArgs) (any, error) {
				executions++
				if mode == "execution" {
					return nil, errors.New("execution failure")
				}
				return map[string]any{"ok": true}, nil
			})
			record := r.Invoke(context.Background(), Execution{}, "bounded", []byte(`{"first":100}`))
			result := decodeClippingResult(t, record)
			if len(result.Warnings) != 1 || validations != 1 {
				t.Fatalf("result=%s validations=%d", record.Result, validations)
			}
			if mode == "validation" {
				if executions != 0 || result.Error == nil || result.Error.Code != "invalid_input" || result.Error.Message != "semantic failure" {
					t.Fatalf("result=%s executions=%d", record.Result, executions)
				}
			} else if executions != 1 || result.OK != (mode == "success") {
				t.Fatalf("result=%s executions=%d", record.Result, executions)
			}
			omitted := r.Invoke(context.Background(), Execution{}, "bounded", []byte(`{}`))
			if strings.Contains(string(omitted.Result), `"warnings"`) {
				t.Fatalf("omission warned: %s", omitted.Result)
			}
			before := validations
			for _, input := range []string{`{"first":"100"}`, `{"first":1.5}`, `{"first":true}`, `{"first":{}}`, `{"first":100,"unknown":1}`, `{"first":1e100}`, `{"first":100} {}`} {
				bad := r.Invoke(context.Background(), Execution{}, "bounded", []byte(input))
				result := decodeClippingResult(t, bad)
				if result.Error == nil || result.Error.Code != "invalid_input" || len(result.Warnings) != 0 || validations != before {
					t.Fatalf("input=%s result=%s validations=%d", input, bad.Result, validations)
				}
			}
		})
	}
}

func TestClippingDecodedFields(t *testing.T) {
	r := NewRegistry()
	Register(r, "bounded", "", map[string]any{
		"first": IntegerRange(1, 10), "second": IntegerRange(-5, 5),
		"text": Property("string"),
	}, nil, func(clippingArgs) error { return nil }, func(context.Context, Execution, clippingArgs) (any, error) { return nil, nil })
	codec, _ := r.Get("bounded")
	for _, tc := range []struct {
		name, input string
		first       *int
		second      *int
		requested   []int
	}{
		{"uppercase", `{"FIRST":99,"SECOND":-99}`, new(10), new(-5), []int{99, -99}},
		{"casefold", `{"FiRsT":-99,"ſeCoNd":99}`, new(1), new(5), []int{-99, 99}},
		{"duplicate last folded clipped", `{"first":3,"FIRST":99}`, new(10), nil, []int{99}},
		{"duplicate last exact valid", `{"FIRST":99,"first":3}`, new(3), nil, nil},
		{"duplicate last exact clipped", `{"FIRST":3,"first":-99}`, new(1), nil, []int{-99}},
		{"duplicate last folded valid", `{"first":99,"FIRST":3}`, new(3), nil, nil},
		{"pointer omitted", `{"first":3}`, new(3), nil, nil},
		{"pointer null", `{"first":3,"second":null}`, new(3), nil, nil},
		{"pointer last null", `{"first":3,"second":99,"SECOND":null}`, new(3), nil, nil},
		{"pointer last nonnull", `{"first":3,"SECOND":null,"second":99}`, new(3), new(5), []int{99}},
		{"all omitted", `{}`, nil, nil, nil},
		{"explicit zero omitempty", `{"first":0}`, new(1), nil, []int{0}},
		{"null", `{"first":null}`, nil, nil, nil},
		{"null clears decoded value", `{"first":99,"FIRST":null}`, nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call, err := codec.DecodeCall(1, []byte(tc.input))
			if err != nil {
				t.Fatal(err)
			}
			args := call.(typedCall[clippingArgs]).args
			if !reflect.DeepEqual(args.First, tc.first) || !reflect.DeepEqual(args.Second, tc.second) {
				t.Fatalf("decoded args = %+v, want first=%v second=%v", args, tc.first, tc.second)
			}
			warnings := call.InputWarnings()
			if len(warnings) != len(tc.requested) {
				t.Fatalf("warnings = %+v, want requested %v", warnings, tc.requested)
			}
			for i, requested := range tc.requested {
				if warnings[i].Requested != requested {
					t.Fatalf("warnings = %+v, want requested %v", warnings, tc.requested)
				}
			}
		})
	}
	text := strings.Repeat("x", 1<<20)
	call, err := codec.DecodeCall(1, []byte(`{"first":99,"text":"`+text+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	args := call.(typedCall[clippingArgs]).args
	if args.First == nil || *args.First != 10 || args.Text != text || len(call.InputWarnings()) != 1 {
		t.Fatal("large unrelated string changed or prevented clipping")
	}
}

func registerClippingTest[A any](properties map[string]any, normalize ...func(*A) []InputWarning) {
	Register(NewRegistry(), "bounded", "", properties, nil, func(A) error { return nil }, func(context.Context, Execution, A) (any, error) { return nil, nil }, normalize...)
}

func TestClippingRegistrationErrors(t *testing.T) {
	for name, register := range map[string]func(){
		"nonstruct": func() { registerClippingTest[int](map[string]any{"first": IntegerRange(1, 10)}) },
		"missing":   func() { registerClippingTest[clippingArgs](map[string]any{"missing": IntegerRange(1, 10)}) },
		"nonint":    func() { registerClippingTest[clippingArgs](map[string]any{"exact": IntegerRange(1, 10)}) },
		"scalar": func() {
			registerClippingTest[struct {
				First int `json:"first,omitempty"`
			}](map[string]any{"first": IntegerRange(1, 10)})
		},
		"unexported": func() {
			registerClippingTest[struct{ first int }](map[string]any{"first": IntegerRange(1, 10)})
		},
		"multiple normalizers": func() { registerClippingTest[clippingArgs](nil, nil, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected programmer-error panic")
				}
			}()
			register()
		})
	}
}

func BenchmarkClippingLargeUnrelatedString(b *testing.B) {
	r := NewRegistry()
	Register(r, "bounded", "", map[string]any{"second": IntegerRange(1, 10), "text": Property("string")}, nil,
		func(clippingArgs) error { return nil }, func(context.Context, Execution, clippingArgs) (any, error) { return nil, nil })
	codec, _ := r.Get("bounded")
	text := strings.Repeat("x", 1<<20)
	for _, tc := range []struct{ name, field string }{
		{"omitted", ""}, {"valid", `,"second":5`}, {"clipped", `,"second":99`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			input := []byte(`{"text":"` + text + `"` + tc.field + `}`)
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			b.ResetTimer()
			for b.Loop() {
				if _, err := codec.DecodeCall(1, input); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestClippingPreservesOutputAndFilePresentation(t *testing.T) {
	r := NewRegistry()
	files := []llm.BinaryFile{{}}
	Register(r, "bounded", "", map[string]any{"first": IntegerRange(1, 10)}, nil, func(clippingArgs) error { return nil }, func(context.Context, Execution, clippingArgs) (any, error) {
		return Output{Value: fileResult{value: map[string]any{"ok": true}, summary: "custom-summary", detail: "custom-detail"}, Files: files}, nil
	})
	record := r.Invoke(context.Background(), Execution{}, "bounded", []byte(`{"first":100}`))
	if !reflect.DeepEqual(record.Files, files) || !strings.Contains(string(record.Markdown.Summary), "custom-summary") || !strings.HasPrefix(string(record.Markdown.Detail), "custom-detail") || len(decodeClippingResult(t, record).Warnings) != 1 {
		t.Fatalf("output lost: %+v", record)
	}
}

func TestClippingTypedNormalizer(t *testing.T) {
	r := NewRegistry()
	Register(r, "dynamic", "", map[string]any{"first": Property("integer")}, nil, func(a clippingArgs) error {
		if a.First == nil || *a.First != 10 {
			t.Fatalf("validator saw %v", a.First)
		}
		return nil
	}, func(context.Context, Execution, clippingArgs) (any, error) {
		return map[string]any{"ok": true}, nil
	}, func(a *clippingArgs) []InputWarning { return ClipInteger("first", a.First, 1, 10) })
	record := r.Invoke(context.Background(), Execution{}, "dynamic", []byte(`{"first":100}`))
	if result := decodeClippingResult(t, record); !result.OK || len(result.Warnings) != 1 || result.Warnings[0].Effective != float64(10) {
		t.Fatal(string(record.Result))
	}
}

func TestClippingWarningsSurviveEncodingAndLargeExecutionResults(t *testing.T) {
	for _, mode := range []string{"encoding", "large"} {
		t.Run(mode, func(t *testing.T) {
			r := NewRegistry()
			Register(r, "bounded", "", map[string]any{"first": IntegerRange(1, 10)}, nil,
				func(clippingArgs) error { return nil }, func(context.Context, Execution, clippingArgs) (any, error) {
					if mode == "encoding" {
						return nil, &Error{Code: "synthetic", Message: "fixture failure", Details: make(chan int)}
					}
					return map[string]any{"ok": true, "content": strings.Repeat("x", 70<<10)}, nil
				})
			record := r.Invoke(context.Background(), Execution{}, "bounded", []byte(`{"first":99}`))
			result := decodeClippingResult(t, record)
			if mode == "encoding" {
				if result.OK || result.Error == nil || result.Error.Code != "execution_failed" {
					t.Fatal(string(record.Result))
				}
			} else if !result.OK || len(record.Result) <= 64<<10 {
				t.Fatal("registry replaced original before runtime delivery")
			}
			if len(result.Warnings) != 1 || result.Warnings[0].Effective != float64(10) {
				t.Fatal("execution record lost input warning")
			}
		})
	}
}
