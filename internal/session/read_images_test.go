package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	contextbuild "ttc/internal/context"
	"ttc/internal/provider"
	"ttc/internal/provider/openai"
)

func readImageFixture(t *testing.T, r *Runtime) (string, string) {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, 257, 257))
	state := uint32(12345)
	for i := range im.Pix {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		im.Pix[i] = byte(state)
	}
	var data bytes.Buffer
	if err := png.Encode(&data, im); err != nil {
		t.Fatal(err)
	}
	if data.Len() <= 64<<10 {
		t.Fatal("fixture must exceed the textual tool-result cap")
	}
	path := filepath.Join(r.Workspace.Root, "source.png")
	if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data.Bytes())
	return path, hex.EncodeToString(hash[:])
}

func TestImageReadReferencesSurviveHistoryLoadAndContinuation(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	r.selection.Model.Images = true
	path, checksum := readImageFixture(t, r)
	calls := []provider.ToolCall{{ID: "image-call", Name: "read", Arguments: json.RawMessage(`{"path":"source.png"}`)}}
	turn, ids := batchIntents(t, r, "main", calls)
	records, err := r.runToolBatch(context.Background(), turn, "main", r.Tools, calls, ids, nil)
	if err != nil || len(records) != 1 || len(records[0].Images) != 1 {
		t.Fatalf("read image failed: %+v, %v", records, err)
	}
	want := provider.Image{Path: path, SHA256: checksum}
	check := func(session string) {
		t.Helper()
		messages, err := r.Store.Messages(session)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, message := range messages {
			if message.Role == "tool" && message.CallID == "image-call" {
				found = true
				if !reflect.DeepEqual(message.Images, []provider.Image{want}) || strings.Contains(message.Content, "data_url") {
					t.Fatalf("history lost the image reference: %+v", message)
				}
			}
		}
		if !found {
			t.Fatal("history lost image tool result")
		}
		withoutImages := append([]provider.Message(nil), messages...)
		for i := range withoutImages {
			withoutImages[i].Images = nil
		}
		if delta := contextbuild.Tokens(messages) - contextbuild.Tokens(withoutImages); delta != 4096 {
			t.Fatal("image context estimate missing", delta)
		}
	}
	original := r.Current()
	check(original)
	loaded, err := r.Store.Load(original)
	if err != nil {
		t.Fatal(err)
	}
	check(loaded.ID)
	archive, err := r.Store.ArchiveTranscript(original, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{archive, archive + ".jsonl"} {
		data, err := os.ReadFile(file)
		if err != nil || !bytes.Contains(data, []byte(checksum)) || bytes.Contains(data, []byte("data_url")) || bytes.Contains(data, []byte("base64")) {
			t.Fatalf("archive must retain references, not pixels: %s, %v", file, err)
		}
	}
	entries, err := r.Store.Branch(original, 0)
	if err != nil {
		t.Fatal(err)
	}
	var retainFrom int64
	for _, entry := range entries {
		if entry.Visible && entry.Role == "assistant" {
			retainFrom = entry.ID
			break
		}
	}
	continued, err := r.Store.Continue(original, "Image inspected.", archive, retainFrom, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	check(continued.ID)
	var payloadRecords int
	if err := r.Store.DB.QueryRow(`SELECT count(*) FROM tool_records WHERE record_json LIKE '%"data_url"%' OR record_json LIKE '%base64%'`).Scan(&payloadRecords); err != nil || payloadRecords != 0 {
		t.Fatal("image payload stored in DB", payloadRecords, err)
	}
	if _, err := os.Stat(filepath.Join(r.Store.Root, "lineages", original, "images")); !os.IsNotExist(err) {
		t.Fatal("image read copied source into history assets", err)
	}
}

func TestImageReadUsesProducingRequestVisionCapability(t *testing.T) {
	for _, vision := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonvision", true: "vision"}[vision], func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			r.selection.Model.Images = vision
			readImageFixture(t, r)
			calls := []provider.ToolCall{{ID: "read", Name: "read", Arguments: json.RawMessage(`{"path":"source.png"}`)}}
			turn, ids := batchIntents(t, r, "main", calls)
			// Picker changes cannot change the capability of an admitted call.
			r.selection.Model.Images = !vision
			records, err := r.runToolBatch(context.Background(), turn, "main", r.Tools, calls, ids, nil)
			if err != nil {
				t.Fatal(err)
			}
			if vision {
				if len(records[0].Images) != 1 || !strings.Contains(string(records[0].Result), `"ok":true`) {
					t.Fatalf("vision read used later picker state: %s", records[0].Result)
				}
			} else if len(records[0].Images) != 0 || !strings.Contains(string(records[0].Result), "unsupported_image_input") {
				t.Fatalf("nonvision read used later picker state: %s", records[0].Result)
			}
		})
	}
}

func TestImageReadChangedOrMissingSourceSurfacesBeforeHTTP(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed", true: "missing"}[missing], func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			path, _ := readImageFixture(t, r)
			auth := filepath.Join(t.TempDir(), "auth.json")
			credentials, err := json.Marshal(openai.Credentials{AuthMode: "chatgpt", Tokens: openai.Tokens{Access: "mock-token", AccountID: "mock-account"}, LastRefresh: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(auth, credentials, 0600); err != nil {
				t.Fatal(err)
			}
			requests := 0
			adapter := openai.New(auth)
			adapter.BaseURL = "http://mock.invalid"
			adapter.Client = mockHTTPClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				step := requests
				requests++
				if step == 0 {
					streamMockResponse(w, "image", step, []mockCall{{"read", map[string]string{"path": "source.png"}}}, "")
				} else {
					streamMockResponse(w, "image", step, nil, "Image received.")
				}
			}))
			r.Provider = adapter
			r.selection.Provider = "openai"
			r.selection.Model.Images = true
			if err := r.Run(&provider.Message{Role: "user", Content: "Read the image."}); err != nil || requests != 2 {
				t.Fatal("initial image exchange failed", err, requests)
			}
			want := "checksum mismatch"
			if missing {
				want = "no such file"
				err = os.Remove(path)
			} else {
				err = os.WriteFile(path, []byte("changed locally"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = r.Run(&provider.Message{Role: "user", Content: "Continue using the image."})
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), path) || requests != 2 {
				t.Fatal("missing/changed source did not surface before HTTP", err, requests)
			}
		})
	}
}
