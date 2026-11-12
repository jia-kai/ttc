package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestModelPayloadFraming(t *testing.T) {
	for _, text := range []string{"", "plain", "\ntext\n\n", "\x00\r\t\xff", "<<<ttc", "<<</ttc-123", "<<<ttc-01", strings.Repeat("9", 10000)} {
		got, err := FormatModelPayload(ModelPayload{Metadata: PayloadMetadata{Kind: "test"}, Text: text})
		if err != nil {
			t.Fatal(err)
		}
		id := modelPayloadID(text)
		want := "<<<" + id + ":\nkind: test\n>>>\n" + text + "\n<<</" + id + ">>>"
		if got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	}
}

func TestModelPayloadIDs(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"", "ttc"}, {"ordinary ttc-0", "ttc"}, {"<<<ttc", "ttc-0"},
		{"<<<ttc-123 <<<ttc-0", "ttc-2"}, {"<<</ttc-01", "ttc-1"},
		{"<<<ttc-0 <<<ttc-1 <<<ttc-2 <<<ttc-3", "ttc-4"},
		{"<<<ttc-" + strings.Repeat("9", 10000), "ttc-0"},
	} {
		if got := modelPayloadID(tc.text); got != tc.want {
			t.Fatalf("%q: %s != %s", tc.text, got, tc.want)
		}
	}
	// Every canonical prefix is occupied, not merely the whole decimal token.
	for _, prefix := range []int{1, 12, 123} {
		text := "<<<ttc-123"
		for i := 0; i < prefix; i++ {
			if i != 1 && i != 12 {
				text += " <<<ttc-" + jsonNumber(i)
			}
		}
		if got := modelPayloadID(text); got == "ttc-"+jsonNumber(prefix) {
			t.Fatal("prefix not blocked", prefix)
		}
	}
}
func jsonNumber(i int) string { b, _ := json.Marshal(i); return string(b) }

func FuzzModelPayloadID(f *testing.F) {
	for _, seed := range []string{"", "<<<ttc-8", "<<<ttc-123 <<<ttc-0", "<<</ttc-01", "<<<ttc-999999999999999999999999999999999999999"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 8192 {
			t.Skip()
		}
		want := "ttc"
		for n := 0; strings.Contains(text, "<<<"+want) || strings.Contains(text, "<<</"+want); n++ {
			want = "ttc-" + jsonNumber(n)
		}
		if got := modelPayloadID(text); got != want {
			t.Fatalf("got %s want %s for %q", got, want, text)
		}
	})
}

func TestNotificationPayloadProjection(t *testing.T) {
	m := Message{Type: TypeRuntimeNotification, Content: `{"type":"job_exit","job_id":"j","stdout":"  out\n","stderr":"err","stdout_delivery_truncated":true,"unknown":{"n":18446744073709551616},"answer_truncated":true}`}
	got, err := m.ModelContent()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"stdout:\n  payload_index: 0", "stderr:\n  payload_index: 1", "kind: job_output", "job_id: j", "stream: stdout", "delivery_truncated: true", ">>>\n  out\n\n<<</ttc>>>", "!!int 18446744073709551616"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "stdout: |") {
		t.Fatal(got)
	}
}

func TestNotificationEmptyStreamsRemainMetadata(t *testing.T) {
	for _, tc := range []struct {
		content string
		blocks  int
		field   string
	}{
		{`{"type":"job_exit","stdout":"","stderr":""}`, 0, ""},
		{`{"type":"job_exit","stdout":"","stderr":"error output"}`, 1, "stderr"},
		{`{"type":"job_exit","stdout":"output","stderr":""}`, 1, "stdout"},
		{`{"type":"child_notification","answer":"","stdout":"","stderr":""}`, 1, "answer"},
	} {
		text, err := (Message{Type: TypeRuntimeNotification, Content: tc.content}).ModelContent()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(text, "<<<ttc:"); got != tc.blocks {
			t.Fatalf("got %d blocks, want %d: %s", got, tc.blocks, text)
		}
		metadata := modelYAML(t, text, "runtime_notification")
		for _, stream := range []string{"stdout", "stderr"} {
			if stream != tc.field && metadata[stream] != "" {
				t.Fatalf("empty %s changed: %+v", stream, metadata)
			}
		}
		if tc.blocks != 0 {
			if !strings.Contains(text, "field: "+tc.field) || !strings.Contains(text, "payload_index: 0") {
				t.Fatal("wrong payload reference", text)
			}
		}
	}
}

func TestNotificationChildPayloadAndStructuredErrors(t *testing.T) {
	m := Message{Type: TypeRuntimeNotification, Content: `{"type":"child_notification","actor_id":"main","child_id":"child","job_id":"job","message":"\n<<<ttc-01\n  raw\t\n","message_delivery_truncated":true,"error":{"code":4,"details":[null,[],{}]},"warning":null,"answer_truncated":true}`}
	original := m
	got, err := m.ModelContent()
	if err != nil {
		t.Fatal(err)
	}
	if m.Content != original.Content || m.Type != original.Type {
		t.Fatal("projection mutated canonical message")
	}
	metadata := modelYAML(t, got, "runtime_notification")
	if metadata["warning"] != nil || metadata["error"] == nil || metadata["answer_truncated"] != true {
		t.Fatal(metadata)
	}
	for _, want := range []string{"<<<ttc-1:", "type: child_notification", "actor_id: main", "child_id: child", "job_id: job", "delivery_truncated: true", ">>>\n\n<<<ttc-01\n  raw\t\n\n<<</ttc-1>>>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
}

func TestNotificationOmittedOutputPreservesRecoveryReference(t *testing.T) {
	for _, omitted := range []bool{false, true} {
		content, err := json.Marshal(struct {
			Type                    string `json:"type"`
			EventSeq                int    `json:"event_seq"`
			OutputOmitted           bool   `json:"output_omitted"`
			Answer                  string `json:"answer"`
			AnswerDeliveryTruncated bool   `json:"answer_delivery_truncated"`
		}{"child_notification", 42, omitted, "answer prefix", omitted})
		if err != nil {
			t.Fatal(err)
		}
		text, err := (Message{Type: TypeRuntimeNotification, Content: string(content), EventSeq: 42}).ModelContent()
		if err != nil {
			t.Fatal(err)
		}
		metadata := modelYAML(t, text, "runtime_notification")
		seq, present := metadata["event_seq"]
		if present != omitted || omitted && seq != 42 {
			t.Fatalf("omitted=%t: invalid recovery metadata %+v", omitted, metadata)
		}
		if !strings.Contains(text, ">>>\nanswer prefix\n<<</ttc>>>") {
			t.Fatal("lost answer prefix", text)
		}
	}
}

func TestRuntimeInstructionPayloadProjection(t *testing.T) {
	for _, kind := range []string{"runtime_context", "runtime_context_update"} {
		fields := `"project":{"instructions":[{"path":"/repo/AGENTS.md","content":"  raw\n\ttext\n","unknown":123}],"available_skills":[]}`
		raw := `{"type":"` + kind + `",` + fields + `}`
		if kind == "runtime_context_update" {
			raw = `{"type":"` + kind + `","set":{` + fields + `}}`
		}
		got, err := (Message{Type: TypeRuntimeContext, Content: raw}).ModelContent()
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"payload_index: 0", "unknown: 123", "kind: project_instruction", "scope: " + kind, ">>>\n  raw\n\ttext\n\n<<</ttc>>>"} {
			if !strings.Contains(got, want) {
				t.Fatalf("missing %q: %s", want, got)
			}
		}
	}
	for _, value := range []string{"[]", "null"} {
		got, err := (Message{Type: TypeRuntimeContext, Content: `{"type":"runtime_context_update","set":{"project":{"instructions":` + value + `}}}`}).ModelContent()
		if err != nil || !strings.Contains(got, "instructions: "+value) || strings.Contains(got, "<<<") {
			t.Fatal(got, err)
		}
	}
}

func payloadMetadataForTest(t *testing.T, text string) map[string]any {
	t.Helper()
	_, rest, ok := strings.Cut(text, ":\n")
	if !ok {
		t.Fatal(text)
	}
	metadata, _, ok := strings.Cut(rest, ">>>\n")
	if !ok {
		t.Fatal(text)
	}
	var got map[string]any
	if err := yaml.Unmarshal([]byte(metadata), &got); err != nil {
		t.Fatal(err)
	}
	return got
}
