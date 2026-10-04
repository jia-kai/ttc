package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/provider"
)

func originalFileImage(t *testing.T, name string, data []byte) provider.Image {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return provider.Image{Path: path, SHA256: hex.EncodeToString(sum[:])}
}

func TestFileImagesInMessagesAndToolOutputs(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"user", "tool"} {
		t.Run(role, func(t *testing.T) {
			im := originalFileImage(t, "not-a-png-extension", data.Bytes())
			selection := provider.Selection{Provider: "openai", Model: provider.ScriptModel()}
			selection.Model.Images = true
			req := provider.Request{ConversationID: "file-images", Selection: selection, Messages: []provider.Message{{Role: role, CallID: "read-file", Content: "image", Images: []provider.Image{im}}}}
			body, err := wire(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct{ Input []map[string]any }
			if err := json.Unmarshal(body, &decoded); err != nil || len(decoded.Input) != 1 {
				t.Fatal("invalid wire input", string(body), err)
			}
			field := "content"
			if role == "tool" {
				field = "output"
			}
			want := []any{
				map[string]any{"type": "input_text", "text": "image"},
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data.Bytes())},
			}
			if !reflect.DeepEqual(decoded.Input[0][field], want) {
				t.Fatal("file image was not resolved", string(body))
			}
			if req.Messages[0].Images[0].DataURL != "" {
				t.Fatal("transport payload mutated canonical image")
			}
			if err := os.WriteFile(im.Path, []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
			if body, err := wire(context.Background(), req); err == nil || body != nil || !strings.Contains(err.Error(), "checksum mismatch") {
				t.Fatal("changed source did not fail reconstruction", err)
			}
			if err := os.Remove(im.Path); err != nil {
				t.Fatal(err)
			}
			if body, err := wire(context.Background(), req); !errors.Is(err, os.ErrNotExist) || body != nil {
				t.Fatal("missing source did not fail reconstruction", err)
			}
		})
	}
}

func TestStreamImageReconstructionFailsBeforeAuth(t *testing.T) {
	a := New("not-a-credential-file")
	selection := provider.Selection{Provider: "openai", Model: provider.ScriptModel()}
	selection.Model.Images = true
	req := provider.Request{ConversationID: "image-tools", Selection: selection, Messages: []provider.Message{{Role: "tool", CallID: "read", Images: []provider.Image{{}}}}}
	emit := func(provider.StreamEvent) error { t.Fatal("unexpected output"); return nil }
	if err := a.Stream(context.Background(), req, emit); err == nil || !strings.Contains(err.Error(), "file image requires") {
		t.Fatal("invalid image reached authentication", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Stream(ctx, req, emit); !errors.Is(err, context.Canceled) {
		t.Fatal("request assembly lost cancellation", err)
	}
}

func TestNativeImageToolOutputsPreserveBytesAndCallAssociation(t *testing.T) {
	// A wide PNG and a non-animated GIF must remain original bytes, not be
	// resized or flattened before reaching the provider.
	var pngBytes, gifBytes bytes.Buffer
	m := image.NewNRGBA(image.Rect(0, 0, 2049, 40))
	state := uint32(1)
	for i := range m.Pix {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		m.Pix[i] = byte(state)
	}
	if err := png.Encode(&pngBytes, m); err != nil {
		t.Fatal(err)
	}
	if pngBytes.Len() <= 64<<10 {
		t.Fatal("fixture is too small")
	}
	frame := image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})
	if err := gif.Encode(&gifBytes, frame, nil); err != nil {
		t.Fatal(err)
	}
	images := []provider.Image{
		originalFileImage(t, "wide.png", pngBytes.Bytes()),
		originalFileImage(t, "static.gif", gifBytes.Bytes()),
	}
	urls := []string{
		"data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes.Bytes()),
		"data:image/gif;base64," + base64.StdEncoding.EncodeToString(gifBytes.Bytes()),
	}
	for _, nativeReplay := range []bool{false, true} {
		t.Run(map[bool]string{false: "canonical-calls", true: "replayed-calls"}[nativeReplay], func(t *testing.T) {
			selection := provider.Selection{Provider: "openai", Model: provider.ScriptModel()}
			selection.Model.Images = true
			calls := []provider.ToolCall{
				{ID: "read-image", Name: "read", Arguments: json.RawMessage(`{"path":"wide.png"}`)},
				{ID: "read-text", Name: "read", Arguments: json.RawMessage(`{"path":"notes.txt"}`)},
				{ID: "read-other-image", Name: "read", Arguments: json.RawMessage(`{"path":"static.gif"}`)},
			}
			assistant := provider.Message{Role: "assistant", Calls: calls}
			if nativeReplay {
				for _, call := range calls {
					raw, err := json.Marshal(map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": string(call.Arguments)})
					if err != nil {
						t.Fatal(err)
					}
					if err := assistant.AppendState(selection, replayVersion, raw); err != nil {
						t.Fatal(err)
					}
				}
			}
			metadata := `{"path":"wide.png","width":2049,"height":40}`
			// Tool completion order differs from call order. The call ID, not
			// position in the batch, determines which result belongs to a call.
			messages := []provider.Message{
				{Role: "user", Content: "Read the files."},
				assistant,
				{Role: "tool", CallID: "read-other-image", Content: "GIF snapshot", Images: images[1:]},
				{Role: "tool", CallID: "read-text", Content: "Original text output"},
				{Role: "tool", CallID: "read-image", Content: metadata, Images: images},
			}
			body, err := wire(context.Background(), provider.Request{ConversationID: "image-tools", Selection: selection, Messages: messages})
			if err != nil {
				t.Fatal(err)
			}
			var request struct{ Input []map[string]any }
			if err := json.Unmarshal(body, &request); err != nil {
				t.Fatal(err)
			}
			want := []map[string]any{
				{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Read the files."}}},
			}
			for _, call := range calls {
				want = append(want, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": string(call.Arguments)})
			}
			want = append(want,
				map[string]any{"type": "function_call_output", "call_id": "read-other-image", "output": []any{
					map[string]any{"type": "input_text", "text": "GIF snapshot"},
					map[string]any{"type": "input_image", "image_url": urls[1]},
				}},
				map[string]any{"type": "function_call_output", "call_id": "read-text", "output": "Original text output"},
				map[string]any{"type": "function_call_output", "call_id": "read-image", "output": []any{
					map[string]any{"type": "input_text", "text": metadata},
					map[string]any{"type": "input_image", "image_url": urls[0]},
					map[string]any{"type": "input_image", "image_url": urls[1]},
				}},
			)
			// Exact equality also excludes synthetic user messages, explicit
			// detail settings, filesystem paths and alternative image encodings.
			if !reflect.DeepEqual(request.Input, want) {
				t.Fatalf("unexpected native tool outputs:\n%s", body)
			}
		})
	}
}

func TestToolOutputTextRepresentation(t *testing.T) {
	for _, content := range []string{"", "text-only", `{"ok":true}`} {
		for _, hasImage := range []bool{false, true} {
			selection := provider.Selection{Provider: "openai", Model: provider.ScriptModel()}
			selection.Model.Images = hasImage
			m := provider.Message{Role: "tool", CallID: "call", Content: content}
			if hasImage {
				m.Images = []provider.Image{{DataURL: "data:image/png;base64,original"}}
			}
			body, err := wire(context.Background(), provider.Request{ConversationID: "image-tools", Selection: selection, Messages: []provider.Message{m}})
			if err != nil {
				t.Fatal(err)
			}
			var request struct{ Input []map[string]any }
			if err := json.Unmarshal(body, &request); err != nil {
				t.Fatal(err)
			}
			var output any = content
			if hasImage {
				output = []any{map[string]any{"type": "input_text", "text": content}, map[string]any{"type": "input_image", "image_url": m.Images[0].DataURL}}
			}
			if len(request.Input) != 1 || !reflect.DeepEqual(request.Input[0]["output"], output) {
				t.Fatalf("wrong tool text representation for %q (image=%t): %s", content, hasImage, body)
			}
		}
	}
}

func TestImagesRejectedForNonvisionModelsBeforeReplay(t *testing.T) {
	for _, role := range []string{"user", "tool", "assistant"} {
		for _, stateModel := range []string{"", "scripted", "foreign-model"} {
			t.Run(role+"/"+stateModel, func(t *testing.T) {
				selection := provider.Selection{Provider: "openai", Model: provider.ScriptModel()}
				selection.Model.Images = false
				m := provider.Message{Role: role, CallID: "call", Images: []provider.Image{{DataURL: "data:image/png;base64,original"}}}
				if stateModel != "" {
					m.State = &provider.ReplayState{Provider: "openai", Model: stateModel, Version: replayVersion, Items: []json.RawMessage{json.RawMessage(`{"type":"reasoning"}`)}}
				}
				body, err := wire(context.Background(), provider.Request{ConversationID: "image-tools", Selection: selection, Messages: []provider.Message{m}})
				if err == nil || !strings.Contains(err.Error(), "model does not support images") || body != nil {
					t.Fatalf("image bypassed model restriction: %s, %v", body, err)
				}
			})
		}
	}
}

func TestReplayStateDoesNotSilentlyDiscardCanonicalImages(t *testing.T) {
	selection := provider.Selection{Provider: "openai", Model: provider.ScriptModel()}
	selection.Model.Images = true
	m := provider.Message{Role: "assistant", Images: []provider.Image{{DataURL: "data:image/png;base64,original"}}}
	if err := m.AppendState(selection, replayVersion, json.RawMessage(`{"type":"reasoning"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := wire(context.Background(), provider.Request{ConversationID: "image-tools", Selection: selection, Messages: []provider.Message{m}}); err == nil || !strings.Contains(err.Error(), "replay state does not support canonical images") {
		t.Fatal("canonical image was lost during replay", err)
	}
}
