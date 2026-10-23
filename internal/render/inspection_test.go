package render

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"ttc/internal/llm"
)

func inspectionString(text string) llm.InspectionValue {
	return llm.InspectionValue{Kind: "string", Text: text}
}
func inspectionDoc(fields ...llm.InspectionField) llm.InspectionDocument {
	return llm.InspectionDocument{Sections: []llm.InspectionSection{{Title: "Input", Fields: fields}}}
}

func TestInspectionSimpleFieldsAndScalarLists(t *testing.T) {
	doc := inspectionDoc(
		llm.InspectionField{Name: "name", Value: inspectionString("hello")},
		llm.InspectionField{Name: "enabled", Value: llm.InspectionValue{Kind: "bool", Text: "true"}},
		llm.InspectionField{Name: "number", Value: llm.InspectionValue{Kind: "number", Text: "9007199254740993"}},
		llm.InspectionField{Name: "words", Value: llm.InspectionValue{Kind: "array", Items: []llm.InspectionValue{inspectionString("one"), inspectionString("界"), {Kind: "null"}}}},
		llm.InspectionField{Name: "empty", Value: llm.InspectionValue{Kind: "array"}},
	)
	text := Inspection(doc)
	for _, want := range []string{`name: "hello"`, "enabled: true", "number: 9007199254740993", `words: ["one", "界", null]`, "empty: []"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "[1]") {
		t.Fatal("short scalar list is indexed", text)
	}
	for _, row := range strings.Split(text, "\n") {
		if ansi.StringWidth(row) > 80 {
			t.Fatal("unbounded inline row", row)
		}
	}
}

func TestLongWordListsRemainFlowStyle(t *testing.T) {
	var words []llm.InspectionValue
	for i := 0; i < 20; i++ {
		words = append(words, inspectionString(fmt.Sprintf("w%d", i)))
	}
	doc := inspectionDoc(llm.InspectionField{Name: "extensions", Value: llm.InspectionValue{Kind: "array", Items: words}})
	compact := Inspection(doc)
	if !strings.Contains(compact, `extensions: ["w0", "w1", "w2", … 15 omitted, "w18", "w19"]`) || strings.Contains(compact, "[1]") {
		t.Fatalf("short-word list expanded into indexed entries: %s", compact)
	}
	if ansi.Strip(Structured(compact, false, true)) != compact {
		t.Fatal("highlighting changed the shortened flow list")
	}
	expanded := InspectionExpanded(doc)
	if strings.Contains(expanded, "omitted") || strings.Contains(expanded, "[1]") || !strings.Contains(expanded, `"w19"]`) {
		t.Fatalf("expanded flow list lost words: %s", expanded)
	}
}

func TestInspectionOmissionsTextAndUnmodifiedSource(t *testing.T) {
	items := make([]llm.InspectionValue, 30)
	fields := make([]llm.InspectionField, 30)
	for i := range items {
		items[i] = llm.InspectionValue{Kind: "object", Fields: []llm.InspectionField{{Name: "index", Value: llm.InspectionValue{Kind: "number", Text: fmt.Sprint(i)}}}}
		fields[i] = llm.InspectionField{Name: fmt.Sprint(i), Value: inspectionString("x")}
	}
	long := strings.Repeat("long user message ", 1000) + "\nline two\n"
	doc := inspectionDoc(
		llm.InspectionField{Name: "text", Value: inspectionString(long)},
		llm.InspectionField{Name: "items", Value: llm.InspectionValue{Kind: "array", Items: items}},
		llm.InspectionField{Name: "fields", Value: llm.InspectionValue{Kind: "object", Fields: fields}},
		llm.InspectionField{Name: "binary", Value: llm.InspectionValue{Kind: "opaque", Text: "secret-opaque-bytes"}},
	)
	before := fmt.Sprintf("%#v", doc)
	text := Inspection(doc)
	for _, want := range []string{"22 of 30 items omitted", "18 of 30 fields omitted", "opaque payload: 19 bytes; sha256"} {
		// Multiline text gains only indentation, not escaping or truncation.
		if !strings.Contains(text, strings.ReplaceAll(want, "\n", "\n    ")) {
			t.Errorf("missing %q", want[:min(len(want), 90)])
		}
	}
	expanded := InspectionExpanded(doc)
	if !strings.Contains(expanded, strings.ReplaceAll(strings.TrimSuffix(long, "\n"), "\n", "\n    ")) || strings.Contains(expanded, "omitted") {
		t.Fatal("expanded readable text lost ordinary source")
	}
	if !strings.Contains(text, `index: 29`) || !strings.Contains(text, `"29": "x"`) {
		t.Fatal("latest container entries missing", text)
	}
	if strings.Contains(text, "secret-opaque-bytes") {
		t.Fatal("opaque bytes leaked", text)
	}
	if !reflect.DeepEqual(before, fmt.Sprintf("%#v", doc)) {
		t.Fatal("source mutated")
	}
}

func TestInspectionFlowListStaysWithItsKey(t *testing.T) {
	doc := inspectionDoc(llm.InspectionField{Name: strings.Repeat("界", 27), Value: llm.InspectionValue{Kind: "array", Items: []llm.InspectionValue{inspectionString(strings.Repeat("界", 6)), inspectionString(strings.Repeat("界", 6))}}})
	text := Inspection(doc)
	if !strings.Contains(text, `: ["`) || strings.Contains(text, ":\n") {
		t.Fatal("scalar list moved away from its key instead of wrapping in the viewer", text)
	}
}

func TestStructuredHighlightPreservesPlainSource(t *testing.T) {
	input := "Input\n  words: [\"one\", \"界\", 42, true, null]\n  \"strange:key\": \"value\"\n  text:\n    literal **Markdown** and `code`\n  [… 20 of 30 items omitted; see raw capture]"
	colored := Structured(input, false, true)
	if !strings.Contains(colored, "\x1b[") {
		t.Fatal("no highlighting")
	}
	if got := ansi.Strip(colored); got != input {
		t.Fatalf("source changed\ngot %q\nwant %q", got, input)
	}
	if got := Structured(input, false, false); got != input {
		t.Fatal("no-color changed source", got)
	}
	fragment := "  partial: 42\n  intact: true"
	styled := Structured(fragment, true, true)
	first, rest, _ := strings.Cut(styled, "\n")
	if strings.Contains(first, "\x1b[") || !strings.Contains(rest, "\x1b[") {
		t.Fatal("page continuation styled as new row", styled)
	}
	hostile := "Input\n  text: \u001b[31mred\u001b[0m"
	if strings.Contains(Structured(hostile, false, false), "\x1b") {
		t.Fatal("source control not removed")
	}
}

func TestStructuredPrettifiedJSONHighlightsArrayValuesAndCommas(t *testing.T) {
	source := "Prettified raw JSON\nRequest\n{\n  \"words\": [\n    \"one\",\n    \"two\"\n  ],\n  \"count\": 2,\n  \"enabled\": true\n}\n"
	styled := Structured(source, false, true)
	if ansi.Strip(styled) != source || Structured(source, false, false) != source {
		t.Fatal("JSON highlighting changed displayed source or ignored no-color")
	}
	for _, want := range []string{inspectionColor(`"one"`, AmberColor), inspectionColor("2", CyanColor), inspectionColor("true", LavenderColor)} {
		if !strings.Contains(styled, want) {
			t.Fatalf("prettified JSON token not highlighted: %q", want)
		}
	}
}

func TestInspectionCompactAndExpandedText(t *testing.T) {
	lines := make([]string, 14)
	for i := range lines {
		lines[i] = fmt.Sprintf("line%d: literal prose", i+1)
	}
	multiline := strings.Join(lines, "\n")
	oneLine := strings.Repeat("界", 150)
	doc := inspectionDoc(
		llm.InspectionField{Name: "text", Value: inspectionString(multiline)},
		llm.InspectionField{Name: "long", Value: inspectionString(oneLine)},
		llm.InspectionField{Name: "non-identifier name", Value: inspectionString("value")},
	)
	compact, expanded := Inspection(doc), InspectionExpanded(doc)
	for _, want := range []string{"text: |", "8 of 14 lines omitted", "line1: literal prose", "line14: literal prose", "50 characters omitted", `"non-identifier name": "value"`} {
		if !strings.Contains(compact, want) {
			t.Errorf("compact missing %q: %s", want, compact)
		}
	}
	if strings.Contains(compact, "line7:") {
		t.Fatal("middle text was not folded")
	}
	if !strings.Contains(expanded, strings.Join(lines, "\n    ")) || !strings.Contains(expanded, oneLine) || strings.Contains(expanded, "omitted") {
		t.Fatal("expanded source lost text", expanded)
	}
	if doc.Sections[0].Fields[0].Value.Text != multiline || doc.Sections[0].Fields[1].Value.Text != oneLine {
		t.Fatal("renderer mutated IR")
	}
}

func TestStructuredLiteralBlockAndPageState(t *testing.T) {
	first := "Input\n  text: |\n    ordinary: prose\n    CHILD-HIDDEN-BODY48: sample\n    con"
	styled, state := StructuredPage(first, StructuredState{}, true)
	rows := strings.Split(styled, "\n")
	for _, i := range []int{2, 3, 4} {
		if strings.Contains(rows[i], "\x1b[") {
			t.Fatal("literal prose colored as a key", rows[i])
		}
	}
	second := "tinued: text\n    samples: 48\n  actual: true"
	styled, _ = StructuredPage(second, state, true)
	rows = strings.Split(styled, "\n")
	if strings.Contains(rows[0], "\x1b[") || strings.Contains(rows[1], "\x1b[") || !strings.Contains(rows[2], "\x1b[") {
		t.Fatal("literal block state not carried across page", styled)
	}
	plain, _ := StructuredPage(second, state, false)
	if plain != second {
		t.Fatal("no-color changed source", plain)
	}
	// Even a | marker split across pages must establish the literal block.
	_, split := StructuredPage("Input\n  text: ", StructuredState{}, false)
	styled, _ = StructuredPage("|\n    ordinary: prose\n  real: 42", split, true)
	rows = strings.Split(styled, "\n")
	if strings.Contains(rows[1], "\x1b[") || !strings.Contains(rows[2], "\x1b[") {
		t.Fatal("split literal marker lost", styled)
	}
	standalone := Structured("Input\n  CHILD-HIDDEN-BODY48: prose\n  good_key: true", false, true)
	rows = strings.Split(standalone, "\n")
	if strings.Contains(rows[1], "\x1b[") || !strings.Contains(rows[2], "\x1b[") {
		t.Fatal("key recognition too broad", standalone)
	}
}
