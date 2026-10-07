package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ttc/internal/prompts"
)

func TestServerRequestsWithStringIDsAndReadOnlyEdits(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	_ = server.SetDeadline(time.Now().Add(3 * time.Second))
	c := New(context.Background(), t.TempDir(), client, client)
	defer c.Close()
	reader := bufio.NewReader(server)
	for _, test := range []struct {
		method string
		params any
		want   string
	}{
		{"workspace/configuration", map[string]any{"items": []any{map[string]any{"section": "x"}}}, `"result":[null]`},
		{"workspace/configuration", map[string]any{"items": make([]any, 257)}, `"error":{"code":-32602,"message":"configuration requires at most 256 items"}`},
		{"workspace/configuration", map[string]any{"items": "invalid"}, `"error":{"code":-32602,"message":"configuration requires at most 256 items"}`},
		{"workspace/workspaceFolders", map[string]any{}, `"uri":"file:`},
		{"window/showMessageRequest", map[string]any{}, `"result":null`},
		{"workspace/applyEdit", map[string]any{"edit": map[string]any{}}, `"applied":false`},
		{"client/registerCapability", map[string]any{}, `"error":{"code":-32601,"message":"TTC does not advertise this client capability"}`},
	} {
		data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "server-id", "method": test.method, "params": test.params})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(server, "Content-Length: %d\r\n\r\n%s", len(data), data); err != nil {
			t.Fatal(err)
		}
		data, err = readFrame(reader)
		if err != nil || !strings.Contains(string(data), `"id":"server-id"`) || !strings.Contains(string(data), test.want) {
			t.Fatal(string(data), err)
		}
		if test.method == "workspace/applyEdit" {
			var response struct {
				Result struct {
					Applied       bool   `json:"applied"`
					FailureReason string `json:"failureReason"`
				} `json:"result"`
			}
			if err := json.Unmarshal(data, &response); err != nil {
				t.Fatal(err)
			}
			if response.Result.Applied || response.Result.FailureReason != prompts.LSPReadOnlyEdits {
				t.Fatalf("read-only refusal changed: %s", data)
			}
		}
	}
}

func TestFramingDiagnosticsPreserveExactMessages(t *testing.T) {
	for _, test := range []struct {
		name, frame, want string
	}{
		{"invalid header", "Content-Length: 2\n\n{}", "invalid or oversized LSP header"},
		{"oversized headers", strings.Repeat("X: x\r\n", 1366), "invalid or oversized LSP header"},
		{"unframed stdout", "ordinary stdout\r\n", "expected Content-Length framing"},
		{"duplicate length", "Content-Length: 2\r\nContent-Length: 2\r\n\r\n{}", "duplicate Content-Length"},
		{"oversized content", "Content-Length: 8388609\r\n\r\n", "Content-Length must be 1–8388608 bytes"},
		{"zero length", "Content-Length: 0\r\n\r\n", "Content-Length must be 1–8388608 bytes"},
		{"negative length", "Content-Length: -1\r\n\r\n", "Content-Length must be 1–8388608 bytes"},
		{"invalid length", "Content-Length: invalid\r\n\r\n", "Content-Length must be 1–8388608 bytes"},
		{"missing length", "Content-Type: application/vscode-jsonrpc\r\n\r\n", "missing Content-Length"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := readFrame(bufio.NewReader(strings.NewReader(test.frame))); err == nil || err.Error() != test.want {
				t.Fatalf("readFrame error = %v, want %q", err, test.want)
			}
			input, server := net.Pipe()
			defer server.Close()
			c := New(context.Background(), t.TempDir(), input, io.NopCloser(strings.NewReader(test.frame)))
			defer c.Close()
			c.wg.Wait()
			var failure *Error
			err := c.failure()
			want := "invalid LSP framing; start with exec SERVER, emit no ordinary stdout, and restart the job: " + test.want
			if !errors.As(err, &failure) || failure.Code != "protocol_error" || failure.Message != want || err.Error() != "protocol_error: "+want {
				t.Fatalf("model-facing framing error changed: %v", err)
			}
		})
	}
}

func TestFramingKeepsUnderlyingReadErrors(t *testing.T) {
	for _, test := range []struct {
		frame string
		want  error
	}{
		{"", io.EOF},
		{"Content-Length: 2\r\n\r\n{", io.ErrUnexpectedEOF},
		{strings.Repeat("x", 4096), bufio.ErrBufferFull},
	} {
		if _, err := readFrame(bufio.NewReader(strings.NewReader(test.frame))); !errors.Is(err, test.want) {
			t.Fatalf("readFrame error = %v, want %v", err, test.want)
		}
	}
}

func TestCancellationInterruptsBlockedWriteAndJoinsReader(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	c := New(context.Background(), t.TempDir(), client, client)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := c.notify(ctx, "large message", strings.Repeat("x", 4096)); err == nil {
		t.Fatal("blocked write succeeded")
	}
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reader did not join")
	}
}

func TestDocumentCacheBoundsAndRegularFiles(t *testing.T) {
	c, path, _ := fixture(t, "utf-16", "normal")
	ctx := queryContext(t)
	root := filepath.Dir(path)
	for i := 0; i < 10; i++ {
		path := filepath.Join(root, fmt.Sprintf("file%d.py", i))
		if err := os.WriteFile(path, []byte("x\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Query(ctx, Query{Operation: "hover", Path: path, Line: 1, Column: 1, Limit: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.documents) != 8 || c.documents[0].path != filepath.Join(root, "file2.py") {
		t.Fatal(c.documents)
	}
	large := filepath.Join(root, "large.py")
	f, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxMessageBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readText(ctx, large); err == nil {
		t.Fatal("accepted oversized file")
	}
	if _, err := readText(ctx, root); err == nil {
		t.Fatal("accepted directory")
	}
	if _, err := filePath("https://example.com/file.py"); err == nil {
		t.Fatal("accepted remote URI")
	}
}
