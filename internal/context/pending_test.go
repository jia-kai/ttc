package context

import (
	"reflect"
	"testing"

	"ttc/internal/llm"
)

func TestAppendPendingMessageIdentityAndPurity(t *testing.T) {
	pending := llm.Message{Type: llm.TypeRuntimeContext, RequestID: 17, Content: "Recovery warning"}
	for _, field := range []string{"same", "runtime", "role", "content", "request"} {
		t.Run(field, func(t *testing.T) {
			existing := pending
			switch field {
			case "runtime":
				existing.Type = llm.TypeDeveloperInstruction
			case "role":
				existing.Type = llm.TypeRuntimeNotification
			case "content":
				existing.Content += " changed"
			case "request":
				existing.RequestID++
			}
			backing := []llm.Message{existing, {Type: llm.TypeAssistant, Content: "do not overwrite"}}
			original := append([]llm.Message(nil), backing...)
			result, added := AppendPendingMessage(backing[:1], &pending)
			if added != (field != "same") || !reflect.DeepEqual(backing, original) {
				t.Fatal("incorrect deduplication or mutated input", result, added, backing)
			}
			if added && (len(result) != 2 || !reflect.DeepEqual(result[1], pending)) {
				t.Fatal("pending identity changed", result)
			}
		})
	}
	if result, added := AppendPendingMessage(nil, nil); result != nil || added {
		t.Fatal("nil pending message changed context", result, added)
	}
}
