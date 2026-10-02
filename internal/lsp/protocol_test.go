package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		{"workspace/workspaceFolders", map[string]any{}, `"uri":"file:`},
		{"window/showMessageRequest", map[string]any{}, `"result":null`},
		{"workspace/applyEdit", map[string]any{"edit": map[string]any{}}, `"applied":false`},
		{"client/registerCapability", map[string]any{}, `"code":-32601`},
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
