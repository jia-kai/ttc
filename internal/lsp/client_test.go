package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, encoding, mode string) (*Client, string, *bytes.Buffer) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python fixture requires python3")
	}
	root := t.TempDir()
	path := filepath.Join(root, "sample.py")
	if err := os.WriteFile(path, []byte("a😀éz\nsecond\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, python, "testdata/server.py")
	cmd.Env = append(os.Environ(), "TTC_LSP_ENCODING="+encoding, "TTC_LSP_MODE="+mode)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := new(bytes.Buffer)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := New(ctx, root, input, output)
	t.Cleanup(func() { c.Close(); cancel(); _ = cmd.Wait() })
	return c, path, stderr
}

func TestLocationConversionHonorsDeadlineWithCachedSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "many.go")
	c := &Client{encoding: "utf-16", documents: []document{{path: path, text: strings.Repeat("a\n", 200000)}}}
	var items []any
	for range 500 {
		items = append(items, map[string]any{"uri": fileURI(path), "range": sourceRange{Start: position{199999, 0}, End: position{199999, 1}}})
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	result, err := c.locations(ctx, raw, 0, 500)
	if !errors.Is(err, context.DeadlineExceeded) || result != nil {
		t.Fatalf("cached source conversions ignored deadline: result=%v, error=%v", result != nil, err)
	}
}

// cancelAfterChecks makes scan cancellation deterministic without sleeps or a
// scheduler race. It is used only by synchronous position conversion helpers.
type cancelAfterChecks struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (c *cancelAfterChecks) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestPositionScansCheckCancellationDuringWork(t *testing.T) {
	for _, test := range []struct {
		name   string
		checks int
		run    func(context.Context) error
	}{
		{"physical lines", 3, func(ctx context.Context) error {
			_, err := physicalLine(ctx, strings.Repeat("x\n", 5000), 4999)
			return err
		}},
		{"wire column", 3, func(ctx context.Context) error {
			_, err := wirePosition(ctx, strings.Repeat("x", 5000), 1, 5000, "utf-16")
			return err
		}},
		{"user column", 4, func(ctx context.Context) error {
			_, _, err := userPosition(ctx, strings.Repeat("x", 5000), position{0, 4999}, "utf-16")
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := test.run(&cancelAfterChecks{ctx, cancel, test.checks}); !errors.Is(err, context.Canceled) {
				t.Fatalf("scan ignored cancellation: %v", err)
			}
		})
	}
}

func queryContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestNegotiatedPositionsNavigationAndSynchronization(t *testing.T) {
	for _, encoding := range []string{"utf-8", "utf-16", "utf-32"} {
		t.Run(encoding, func(t *testing.T) {
			c, path, _ := fixture(t, encoding, "normal")
			ctx := queryContext(t)
			q := Query{Operation: "hover", Path: path, Line: 1, Column: 3, Limit: 100}
			result, err := c.Query(ctx, q)
			if err != nil || !strings.Contains(result["markdown"].(string), "a😀éz") {
				t.Fatal(result, err)
			}
			q.Operation = "definition"
			q.Limit = 1
			result, err = c.Query(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			locations := result["locations"].([]Location)
			if len(locations) != 1 || locations[0].StartColumn != 2 || locations[0].EndColumn != 4 || result["next_offset"] != 1 {
				t.Fatal(result)
			}
			q.Operation = "references"
			q.Offset = 2
			result, err = c.Query(ctx, q)
			if err != nil || len(result["locations"].([]Location)) != 1 || result["next_offset"] != nil {
				t.Fatal(result, err)
			}
			q = Query{Operation: "document_symbols", Path: path, Limit: 1}
			result, err = c.Query(ctx, q)
			if err != nil || result["symbols"].([]Symbol)[0].Name != "parent" || result["next_offset"] != 1 {
				t.Fatal(result, err)
			}
			q.Offset = 1
			result, err = c.Query(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			child := result["symbols"].([]Symbol)[0]
			if child.Name != "child" || child.Container != "parent" {
				t.Fatal(child)
			}
			if err := os.WriteFile(path, []byte("a😀é changed\n"), 0600); err != nil {
				t.Fatal(err)
			}
			result, err = c.Query(ctx, Query{Operation: "hover", Path: path, Line: 1, Column: 3, Limit: 100})
			if err != nil || !strings.Contains(result["markdown"].(string), "changed") || c.documents[0].version != 2 {
				t.Fatal(result, err)
			}
		})
	}
}

func TestCanceledQueryIgnoresLateReplyAndKeepsServer(t *testing.T) {
	c, path, _ := fixture(t, "utf-16", "slow")
	// Initialize separately so this deadline tests a query, not initialization.
	if err := c.initialize(queryContext(t)); err != nil {
		t.Fatal(err)
	}
	q := Query{Operation: "hover", Path: path, Line: 1, Column: 1, Limit: 100}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Query(ctx, q); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	result, err := c.Query(queryContext(t), q)
	if err != nil || !strings.HasPrefix(result["markdown"].(string), "**fixture**") {
		t.Fatal(result, err)
	}
}

func TestServerErrorsAndUnsupportedCapabilities(t *testing.T) {
	for mode, code := range map[string]string{"bad_frame": "protocol_error", "error": "invalid_input", "unsupported": "unsupported_operation"} {
		t.Run(mode, func(t *testing.T) {
			c, path, _ := fixture(t, "utf-16", mode)
			_, err := c.Query(queryContext(t), Query{Operation: "hover", Path: path, Line: 1, Column: 1, Limit: 100})
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != code || strings.Contains(failure.Message, "\x1b") {
				t.Fatal(err)
			}
		})
	}
}

func TestWorkspaceSymbolsResolve(t *testing.T) {
	c, path, _ := fixture(t, "utf-8", "resolve")
	ctx := queryContext(t)
	if _, err := c.Query(ctx, Query{Operation: "hover", Path: path, Line: 1, Column: 1, Limit: 100}); err != nil {
		t.Fatal(err)
	}
	result, err := c.Query(ctx, Query{Operation: "workspace_symbols", Text: "resolved", Limit: 100})
	if err != nil || result["symbols"].([]Symbol)[0].Name != "resolved" {
		t.Fatal(result, err)
	}
}

func TestFrameBoundsAndPositionValidation(t *testing.T) {
	for _, frame := range []string{"Content-Length: 9000000\r\n\r\n", "Content-Length: 2\r\nContent-Length: 2\r\n\r\n{}", "Content-Length: 2\n\n{}", "ordinary stdout\n", "Content-Length: 0\r\n\r\n"} {
		if _, err := readFrame(bufio.NewReader(strings.NewReader(frame))); err == nil {
			t.Fatal("accepted", frame)
		}
	}
	data, err := readFrame(bufio.NewReader(strings.NewReader("Content-Type: application/vscode-jsonrpc; charset=utf-8\r\nContent-Length: 2\r\n\r\n{}")))
	if err != nil || string(data) != "{}" {
		t.Fatal(string(data), err)
	}
	for encoding, units := range map[string]int{"utf-8": 5, "utf-16": 3, "utf-32": 2} {
		p, err := wirePosition(context.Background(), "a😀éz\r\n", 1, 3, encoding)
		if err != nil || p.Character != units {
			t.Fatal(p, err)
		}
		_, col, err := userPosition(context.Background(), "a😀éz\r\n", p, encoding)
		if err != nil || col != 3 {
			t.Fatal(col, err)
		}
		_, col, err = userPosition(context.Background(), "a😀éz\r\n", position{0, 999}, encoding)
		if err != nil || col != 5 {
			t.Fatal(col, err)
		}
	}
	if _, _, err := userPosition(context.Background(), "😀", position{0, 1}, "utf-16"); err == nil {
		t.Fatal("accepted split surrogate")
	}
	if _, err := wirePosition(context.Background(), "a", 1, 3, "utf-8"); err == nil {
		t.Fatal("accepted past end")
	}
	if _, err := wirePosition(context.Background(), "a", 2, 1, "utf-8"); err == nil {
		t.Fatal("accepted nonexistent line")
	}
	root := t.TempDir()
	fifo := filepath.Join(root, "pipe.py")
	if err := exec.Command("mkfifo", fifo).Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := readText(context.Background(), fifo); err == nil {
		t.Fatal("accepted FIFO")
	}
}
