package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"ttc/internal/llm"
	"ttc/internal/providers/openai"
)

func TestNativeRequestInspectorUsesItemDeltaWithoutChangingWire(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	var sent [][]byte
	var sentMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sentMu.Lock()
		sent = append(sent, body)
		index := len(sent)
		sentMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Done.\"}\n\n")
		item := map[string]any{"type": "message", "id": fmt.Sprintf("m%d", index), "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Done."}}}
		event, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		fmt.Fprintf(w, "data: %s\n\n", event)
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r%d\"}}\n\n", index)
	}))
	defer server.Close()
	r.mu.Lock()
	r.selection.Provider = "openai"
	r.mu.Unlock()
	r.Provider = openai.NewAdapter(openai.Config{Client: server.Client(), BaseURL: server.URL, TokenSource: func(context.Context) (openai.AccessTokens, error) {
		return openai.AccessTokens{Access: "synthetic-token", AccountID: "synthetic-account"}, nil
	}})
	for _, text := range []string{"First request", "second addition"} {
		if err := r.Run(&llm.Message{Type: llm.TypeUserInput, Content: text}); err != nil {
			t.Fatal(err)
		}
	}
	sentMu.Lock()
	count := len(sent)
	var wire []byte
	if count == 2 {
		wire = sent[1]
	}
	sentMu.Unlock()
	if count != 2 || !strings.Contains(string(wire), "available_skills") {
		t.Fatal("fixture did not send the complete native context prefix")
	}
	items, err := r.Store.StreamItems(context.Background(), r.Current())
	if err != nil || len(items) != 2 {
		t.Fatal(items, err)
	}
	inspection, err := r.Store.ReadInspection(context.Background(), items[1].Revision)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(inspection.Text, "available_skills") || !strings.Contains(inspection.Text, "second addition") {
		t.Fatalf("delta inspector repeated unchanged catalog or lost new input: %s", inspection.Text)
	}
	captures, err := r.Store.BoundaryCaptures(context.Background(), items[1].Revision)
	if err != nil || len(captures) < 1 {
		t.Fatal(captures, err)
	}
	raw, err := r.Store.BoundaryRaw(context.Background(), captures[0].ID)
	if err != nil || string(raw) != string(wire) {
		t.Fatal("delta projection modified the retained complete wire payload", err)
	}
}
