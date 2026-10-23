package openai

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func inspectionField(t *testing.T, fields []llm.InspectionField, name string) llm.InspectionValue {
	t.Helper()
	for _, field := range fields {
		if field.Name == name {
			return field.Value
		}
	}
	t.Fatalf("missing field %q in %+v", name, fields)
	return llm.InspectionValue{}
}

func TestInspectionRequestItemsCanonicalNativeJSON(t *testing.T) {
	payload := json.RawMessage(`{ "z_future":{"z":9007199254740993,"a":1.2300e+04}, "model":"test", "instructions":"keep\n  spacing", "tools":[{"parameters":{"z":false,"a":true},"name":"z","type":"function"},{"name":"a","type":"function"}], "input":[{"role":"user","content":"{\"z\":1, \"a\":2}"},{"type":"function_call","call_id":"unique","name":"z","arguments":"  {\"x\":1}  "}] }`)
	before := append([]byte(nil), payload...)
	doc, err := new(Adapter).InspectBoundary("request", payload)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON := []string{
		`{"model":"test"}`,
		`{"z_future":{"a":1.2300e+04,"z":9007199254740993}}`,
		`{"instructions":"keep\n  spacing"}`,
		`{"name":"z","parameters":{"a":true,"z":false},"type":"function"}`,
		`{"name":"a","type":"function"}`,
		`{"content":"{\"z\":1, \"a\":2}","role":"user"}`,
		`{"arguments":"  {\"x\":1}  ","call_id":"unique","name":"z","type":"function_call"}`,
	}
	wantSections := []string{"Settings", "Settings", "Instructions", "Tool definitions", "Tool definitions", "Input messages", "Input messages"}
	wantNames := []string{"model", "z_future", "instructions", "z", "a", "1 · user", "2 · function_call · z"}
	if len(doc.Items) != len(wantJSON) {
		t.Fatalf("items = %+v", doc.Items)
	}
	for i, item := range doc.Items {
		if string(item.JSON) != wantJSON[i] || !json.Valid(item.JSON) || item.Section != wantSections[i] || item.Name != wantNames[i] {
			t.Errorf("item %d = %+v; JSON = %s", i, item, item.JSON)
		}
		wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte(item.Section+"\x00"+wantJSON[i])))
		if item.Hash != wantHash {
			t.Errorf("item %d hash = %s, want %s", i, item.Hash, wantHash)
		}
	}
	if !reflect.DeepEqual([]byte(payload), before) {
		t.Fatal("parser modified original bytes")
	}
	// Readable values may decode embedded JSON, while native item JSON must
	// retain the original string, including its whitespace and object-key order.
	if got := inspectionField(t, doc.Items[6].Value.Fields, "arguments"); got.Kind != llm.InspectionKindObject {
		t.Fatal(got)
	}
	if doc.Sections != nil {
		t.Fatal("request constructed redundant whole-document sections")
	}
	var event llm.BoundaryEvent
	req := llm.Request{Observe: func(got llm.BoundaryEvent) error { event = got; return nil }}
	if err := new(Adapter).observeBoundary(req, 1, "request", payload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]byte(event.Payload), before) || !reflect.DeepEqual(event.Inspection.Items, doc.Items) {
		t.Fatal("boundary capture or inspection changed")
	}
}

func TestInspectionRequestItemHashesIgnoreKeyOrderAndInputPosition(t *testing.T) {
	parse := func(payload string) []llm.InspectionItem {
		t.Helper()
		doc, err := new(Adapter).InspectBoundary("request", json.RawMessage(payload))
		if err != nil {
			t.Fatal(err)
		}
		return doc.Items
	}
	before := parse(`{"input":[{"role":"user","content":[{"text":"same","type":"input_text"}]},{"role":"user","content":[{"type":"input_text","text":"same"}]}]}`)
	after := parse(`{"input":[{"role":"developer","content":"prefix"},{"content":[{"type":"input_text","text":"same"}],"role":"user"},{"content":[{"text":"same","type":"input_text"}],"role":"user"},{"content":"suffix","role":"assistant"}]}`)
	if len(before) != 2 || len(after) != 4 || before[0].Hash != before[1].Hash || before[0].Hash != after[1].Hash || before[1].Hash != after[2].Hash {
		t.Fatal(before, after)
	}
	if before[0].Name == after[1].Name {
		t.Fatal("name did not shift", before, after)
	}
	if string(before[0].JSON) != string(after[1].JSON) {
		t.Fatal("key order was not normalized")
	}
}

func TestInspectionItemsAvailabilityAndEmptyCollections(t *testing.T) {
	doc, err := new(Adapter).InspectBoundary("request", json.RawMessage(`{}`))
	if err != nil || doc.Items == nil || len(doc.Items) != 0 || doc.Sections != nil {
		t.Fatal(doc, err)
	}
	for _, direction := range []string{"response", "outcome"} {
		doc, err := new(Adapter).InspectBoundary(direction, json.RawMessage(`{}`))
		if err != nil || doc.Items != nil {
			t.Fatal(direction, doc, err)
		}
	}
	doc, err = new(Adapter).InspectBoundary("request", json.RawMessage(`invalid`))
	if err == nil || doc.Items != nil {
		t.Fatal(doc, err)
	}
}

func TestInspectionNativeRequest(t *testing.T) {
	payload := json.RawMessage(`{ "model":"test", "instructions":"Be helpful", "tools":[{"type":"function","name":"read","parameters":{"type":"object"}}], "input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"{\"retained_inputs\":[{\"content\":\"remember\"}]}"}]},{"type":"function_call","name":"read","arguments":"{\"paths\":[\"a\",\"b\"],\"count\":9007199254740993}"}],"future":{"flags":[true,null,1.2300e+04]} }`)
	before := string(payload)
	doc, err := new(Adapter).InspectBoundary("request", payload)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, item := range doc.Items {
		titles = append(titles, item.Section)
	}
	if !reflect.DeepEqual(titles, []string{"Settings", "Settings", "Instructions", "Tool definitions", "Input messages", "Input messages"}) || string(payload) != before {
		t.Fatal(titles, string(payload))
	}
	if doc.Items[1].Name != "model" || doc.Items[1].Value.Text != "test" {
		t.Fatal(doc.Items)
	}
	flags := inspectionField(t, doc.Items[0].Value.Fields, "flags")
	if flags.Kind != llm.InspectionKindArray || len(flags.Items) != 3 || flags.Items[0].Kind != llm.InspectionKindBool || flags.Items[1].Kind != llm.InspectionKindNull || flags.Items[2].Text != "1.2300e+04" {
		t.Fatal(flags)
	}
	if doc.Items[3].Name != "read" || doc.Items[4].Name != "1 · message · user" {
		t.Fatal(doc)
	}
	message := doc.Items[4].Value
	content := inspectionField(t, message.Fields, "content")
	text := inspectionField(t, content.Items[0].Fields, "text")
	retained := inspectionField(t, text.Fields, "retained_inputs")
	if retained.Kind != llm.InspectionKindArray || inspectionField(t, retained.Items[0].Fields, "content").Text != "remember" {
		t.Fatal(retained)
	}
	args := inspectionField(t, doc.Items[5].Value.Fields, "arguments")
	paths := inspectionField(t, args.Fields, "paths")
	if len(paths.Items) != 2 || paths.Items[0].Text != "a" || paths.Items[1].Text != "b" || inspectionField(t, args.Fields, "count").Text != "9007199254740993" {
		t.Fatal(args)
	}
}

func TestInspectionNativeResponseAndOpaque(t *testing.T) {
	doc, err := new(Adapter).InspectBoundary("response", json.RawMessage(`{"type":"function_call_output","output":"{\"ok\":true}","content":"[1,2]","text":"{invalid}","file_data":"{\"secret\":1}","encrypted_content":"[1]","image_url":"data:image/png;base64,AAAA","unknown":{"kept":"yes"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Sections) != 1 || doc.Sections[0].Title != "Response" {
		t.Fatal(doc)
	}
	fields := doc.Sections[0].Fields
	for _, name := range []string{"file_data", "encrypted_content", "image_url"} {
		if got := inspectionField(t, fields, name); got.Kind != llm.InspectionKindOpaque || got.Text == "" {
			t.Fatal(name, got)
		}
	}
	if inspectionField(t, fields, "output").Kind != llm.InspectionKindObject || inspectionField(t, fields, "content").Kind != llm.InspectionKindArray || inspectionField(t, fields, "text").Kind != llm.InspectionKindString || inspectionField(t, inspectionField(t, fields, "unknown").Fields, "kept").Text != "yes" {
		t.Fatal(fields)
	}
}

func TestInspectionValidationAndDepth(t *testing.T) {
	a := new(Adapter)
	for _, payload := range []string{`{}`, `{"status":"success"}`} {
		doc, err := a.InspectBoundary("outcome", json.RawMessage(payload))
		if err != nil || doc.Sections[0].Title != "Outcome" {
			t.Fatal(doc, err)
		}
	}
	for _, payload := range []string{``, `null`, `[]`, `{} {}`, `{} garbage`, `{"x":`} {
		if _, err := a.InspectBoundary("request", json.RawMessage(payload)); err == nil {
			t.Fatalf("accepted %q", payload)
		}
	}
	if _, err := a.InspectBoundary("other", json.RawMessage(`{}`)); err == nil {
		t.Fatal("accepted unsupported direction")
	}
	payload := `{"deep":` + strings.Repeat(`[`, 100) + `0` + strings.Repeat(`]`, 100) + `}`
	doc, err := a.InspectBoundary("response", json.RawMessage(payload))
	if err != nil {
		t.Fatal(err)
	}
	value := doc.Sections[0].Fields[0].Value
	depth := 0
	for value.Kind == llm.InspectionKindArray {
		depth++
		value = value.Items[0]
	}
	if depth > maxInspectionDepth || value.Kind != llm.InspectionKindOpaque || !strings.Contains(value.Text, "0") {
		t.Fatal(depth, value)
	}
}

func TestInspectionObservationIsOptionalAndParseFailurePreservesRaw(t *testing.T) {
	a := new(Adapter)
	if err := a.observeBoundary(llm.Request{}, 1, "request", []byte(`invalid`)); err != nil {
		t.Fatal("unobserved boundary parsed", err)
	}
	calls := 0
	req := llm.Request{Observe: func(event llm.BoundaryEvent) error {
		calls++
		if string(event.Payload) != "invalid" || event.Inspection == nil || event.Inspection.Sections[0].Title != "Inspection unavailable" {
			t.Fatal(event)
		}
		return nil
	}}
	if err := a.observeBoundary(req, 1, "request", []byte(`invalid`)); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
}
