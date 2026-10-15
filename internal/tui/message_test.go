package tui

import (
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestMessageProjectionUsesSemanticIdentity(t *testing.T) {
	payload := `{"type":"child_turn_finished","child_id":"main/child_test","answer":"answer"}`
	human := messageLine(line{}, llm.Message{Type: llm.TypeUserInput, Content: payload})
	if !human.human || human.compactResponse || human.text != "user · "+payload || human.system {
		t.Fatalf("human JSON impersonated runtime: %+v", human)
	}
	runtime := messageLine(line{}, llm.Message{Type: llm.TypeRuntimeNotification, Content: payload})
	if runtime.human || !runtime.compactResponse || runtime.actor != "main/child_test" || runtime.text != "answer" {
		t.Fatalf("typed child result not projected: %+v", runtime)
	}
}

func TestMessageProjectionCategories(t *testing.T) {
	for _, tc := range []struct {
		typeID llm.MessageType
		want   string
		system bool
	}{
		{llm.TypeRuntimeContext, "Runtime context", true},
		{llm.TypeRuntimeNotification, "Runtime notification · job exit · job_1 · completed", true},
		{llm.TypeChildGuidance, "Main-agent guidance", true},
		{llm.TypeDeveloperInstruction, "Instructions", true},
		{llm.TypeRuntimeInstruction, "Runtime instructions", true},
	} {
		t.Run(string(tc.typeID), func(t *testing.T) {
			item := messageLine(line{}, llm.Message{Type: tc.typeID, Content: `{"type":"job_exit","job_id":"job_1","status":"completed"}`})
			if !strings.Contains(item.text, tc.want) || item.system != tc.system || item.human {
				t.Fatalf("projection %+v; want %q system=%v", item, tc.want, tc.system)
			}
		})
	}
}

func TestNotificationPreviewIsBounded(t *testing.T) {
	item := messageLine(line{}, llm.Message{Type: llm.TypeRuntimeNotification, Content: `{"type":"wakeup","message":"` + strings.Repeat("long reminder ", 10000) + `"}`})
	if len([]rune(item.text)) > 256 || !item.brief || !item.system {
		t.Fatalf("unbounded notification preview: bytes=%d brief=%v", len(item.text), item.brief)
	}
}

func TestMessageProjectionPreservesAuthoredAndAssistantText(t *testing.T) {
	authored := "User-authored text"
	item := messageLine(line{}, llm.Message{Type: llm.TypeUserInput, Content: "expanded attachment bytes", UserText: &authored})
	if item.text != "user · "+authored {
		t.Fatalf("display expanded content instead of authored text: %+v", item)
	}
	item = messageLine(line{}, llm.Message{Type: llm.TypeAssistant, Content: "**Full** response", RequestID: 42})
	if item.text != "**Full** response" || item.speaker != "assistant" || !item.markdown || item.requestID != 42 {
		t.Fatalf("assistant projection lost metadata: %+v", item)
	}
}
