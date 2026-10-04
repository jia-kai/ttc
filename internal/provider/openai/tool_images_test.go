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
	"time"

	"ttc/internal/blobcache"
	"ttc/internal/provider"
)

func originalBinaryFile(t *testing.T, name string, data []byte) provider.BinaryFile {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return provider.BinaryFile{Path: path, SHA256: hex.EncodeToString(sum[:])}
}

func TestFileImagesInMessagesAndToolOutputs(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"user", "tool"} {
		t.Run(role, func(t *testing.T) {
			im := originalBinaryFile(t, "not-a-png-extension", data.Bytes())
			selection := provider.Selection{Provider: "openai", Model: provider.ScriptModel()}
			selection.Model.Images = true
			req := provider.Request{ConversationID: "file-images", Selection: selection, Messages: []provider.Message{{Role: role, CallID: "read-file", Content: "image", Files: []provider.BinaryFile{im}}}}
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
			if req.Messages[0].Files[0].DataURL != "" {
				t.Fatal("transport payload mutated canonical image")
			}
			if err := os.WriteFile(im.Path, []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
			if got, err := wire(context.Background(), req); err != nil || !bytes.Equal(got, body) {
				t.Fatal("changed source invalidated cached upload", err)
			}
			cache, err := blobcache.Default()
			if err != nil {
				t.Fatal(err)
			}
			cache.TTL = time.Nanosecond
			if err := cache.Prune(context.Background()); err != nil {
				t.Fatal(err)
			}
			checkNotice := func(reason string) {
				t.Helper()
				got, err := wire(context.Background(), req)
				if err != nil {
					t.Fatal("unavailable image blocked request", err)
				}
				var decoded struct{ Input []map[string]any }
				if err := json.Unmarshal(got, &decoded); err != nil || len(decoded.Input) != 1 {
					t.Fatal("invalid unavailable-image wire", string(got), err)
				}
				parts := decoded.Input[0][field].([]any)
				if len(parts) != 2 || !reflect.DeepEqual(parts[0], map[string]any{"type": "input_text", "text": "image"}) {
					t.Fatal("unavailable image lost original text", string(got))
				}
				part := parts[1].(map[string]any)
				text, _ := part["text"].(string)
				if part["type"] != "input_text" || !strings.Contains(text, "binary file unavailable") || !strings.Contains(text, im.Path) || !strings.Contains(text, im.SHA256) || !strings.Contains(text, reason) || part["image_url"] != nil {
					t.Fatal("missing explicit unavailable-image diagnostic", string(got))
				}
				if role == "tool" && decoded.Input[0]["call_id"] != "read-file" {
					t.Fatal("unavailable image lost tool association", string(got))
				}
				if !reflect.DeepEqual(req.Messages[0].Files, []provider.BinaryFile{im}) || req.Messages[0].Content != "image" {
					t.Fatal("unavailable image mutated history", req.Messages[0])
				}
			}
			checkNotice("checksum mismatch")
			if err := os.Remove(im.Path); err != nil {
				t.Fatal(err)
			}
			checkNotice("no such file")
		})
	}
}

func TestUnavailableBinaryFileDoesNotDiscardOtherAttachments(t *testing.T) {
	var missingBytes, validBytes bytes.Buffer
	if err := png.Encode(&missingBytes, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(&validBytes, image.NewNRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	missing := originalBinaryFile(t, "missing.png", missingBytes.Bytes())
	valid := originalBinaryFile(t, "valid.png", validBytes.Bytes())
	if err := os.Remove(missing.Path); err != nil {
		t.Fatal(err)
	}
	selection := provider.Selection{Provider: "openai", Model: provider.ScriptModel()}
	selection.Model.Images = true
	m := provider.Message{Role: "tool", CallID: "read-both", Content: "Original metadata", Files: []provider.BinaryFile{missing, valid}}
	body, err := wire(context.Background(), provider.Request{ConversationID: "mixed-availability", Selection: selection, Messages: []provider.Message{m}})
	if err != nil {
		t.Fatal(err)
	}
	var request struct{ Input []map[string]any }
	if err := json.Unmarshal(body, &request); err != nil || len(request.Input) != 1 {
		t.Fatal("invalid wire input", string(body), err)
	}
	parts := request.Input[0]["output"].([]any)
	wantImage := map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(validBytes.Bytes())}
	if request.Input[0]["call_id"] != m.CallID || len(parts) != 3 || !reflect.DeepEqual(parts[0], map[string]any{"type": "input_text", "text": m.Content}) || !reflect.DeepEqual(parts[2], wantImage) {
		t.Fatal("unavailable attachment discarded valid image or text", string(body))
	}
	notice := parts[1].(map[string]any)
	if notice["type"] != "input_text" || !strings.Contains(notice["text"].(string), missing.SHA256) {
		t.Fatal("missing unavailable-image notice", string(body))
	}
}

func TestStreamImageReconstructionFailsBeforeAuth(t *testing.T) {
	a := New("not-a-credential-file")
	selection := provider.Selection{Provider: "openai", Model: provider.ScriptModel()}
	selection.Model.Images = true
	req := provider.Request{ConversationID: "image-tools", Selection: selection, Messages: []provider.Message{{Role: "tool", CallID: "read", Files: []provider.BinaryFile{{}}}}}
	emit := func(provider.StreamEvent) error { t.Fatal("unexpected output"); return nil }
	if err := a.Stream(context.Background(), req, emit); err == nil || !strings.Contains(err.Error(), "file-backed binary requires") {
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
	images := []provider.BinaryFile{
		originalBinaryFile(t, "wide.png", pngBytes.Bytes()),
		originalBinaryFile(t, "static.gif", gifBytes.Bytes()),
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
				{Role: "tool", CallID: "read-other-image", Content: "GIF snapshot", Files: images[1:]},
				{Role: "tool", CallID: "read-text", Content: "Original text output"},
				{Role: "tool", CallID: "read-image", Content: metadata, Files: images},
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
				m.Files = []provider.BinaryFile{{DataURL: "data:image/png;base64,b3JpZ2luYWw="}}
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
				output = []any{map[string]any{"type": "input_text", "text": content}, map[string]any{"type": "input_image", "image_url": m.Files[0].DataURL}}
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
				m := provider.Message{Role: role, CallID: "call", Files: []provider.BinaryFile{{DataURL: "data:image/png;base64,b3JpZ2luYWw="}}}
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
	m := provider.Message{Role: "assistant", Files: []provider.BinaryFile{{DataURL: "data:image/png;base64,b3JpZ2luYWw="}}}
	if err := m.AppendState(selection, replayVersion, json.RawMessage(`{"type":"reasoning"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := wire(context.Background(), provider.Request{ConversationID: "image-tools", Selection: selection, Messages: []provider.Message{m}}); err == nil || !strings.Contains(err.Error(), "replay state does not support canonical binary files") {
		t.Fatal("canonical image was lost during replay", err)
	}
}
