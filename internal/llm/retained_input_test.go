package llm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRetainedInputMetadataJSON(t *testing.T) {
	original := Message{Role: "user", InputSource: "queue", InputTimeMS: 1000, RetainedAtMS: 9000, Content: "original"}
	encoded, err := json.Marshal(original)
	if err != nil || !strings.Contains(string(encoded), `"retained_at_ms":9000`) {
		t.Fatal(string(encoded), err)
	}
	var decoded Message
	if err := json.Unmarshal(encoded, &decoded); err != nil || !reflect.DeepEqual(decoded, original) {
		t.Fatal(decoded, err)
	}
	original.RetainedAtMS = 0
	encoded, err = json.Marshal(original)
	if err != nil || strings.Contains(string(encoded), "retained_at_ms") {
		t.Fatal(string(encoded), err)
	}
}
