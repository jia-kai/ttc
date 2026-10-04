// Package lsp implements a bounded, read-only Language Server Protocol client.
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ttc/internal/render"
)

const maxMessageBytes = 8 << 20

// Error identifies a recoverable tool failure. Message describes how to adjust
// the request or restart a stopped/misconfigured language server.
type Error struct {
	Code, Message string
}

func (e *Error) Error() string        { return e.Code + ": " + e.Message }
func fail(code, message string) error { return &Error{code, message} }

type rpcMessage struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Client owns protocol stdin/stdout and one reader goroutine. Queries serialize
// initialization and document updates; replies still match independent RPC IDs.
// Close joins the reader. The surrounding job manager owns the server process.
type Client struct {
	root                   string
	input                  io.WriteCloser
	output                 io.ReadCloser
	ctx                    context.Context
	cancel                 context.CancelFunc
	wg                     sync.WaitGroup
	write                  chan struct{}
	query                  chan struct{}
	mu                     sync.Mutex
	next                   int64
	pending                map[string]chan rpcMessage
	err                    error
	initialized, attempted bool
	encoding               string
	capabilities           map[string]json.RawMessage
	openClose              bool
	change                 int
	documents              []document
}

// New starts a client for one server rooted at an absolute workspace directory.
// Protocol messages and synchronized text files are each capped at 8 MiB.
func New(ctx context.Context, root string, input io.WriteCloser, output io.ReadCloser) *Client {
	ctx, cancel := context.WithCancel(ctx)
	c := &Client{root: root, input: input, output: output, ctx: ctx, cancel: cancel, write: make(chan struct{}, 1), query: make(chan struct{}, 1), pending: map[string]chan rpcMessage{}, encoding: "utf-16"}
	c.wg.Add(1)
	go c.readLoop()
	return c
}

// Close interrupts protocol I/O and joins the reader; repeated calls are safe.
func (c *Client) Close() {
	c.stop(fail("job_not_running", "language server stopped; start a new protocol=lsp job"))
	c.wg.Wait()
}

func (c *Client) stop(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	c.pending = map[string]chan rpcMessage{}
	c.cancel()
	c.mu.Unlock()
	_ = c.input.Close()
	_ = c.output.Close()
}

func (c *Client) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return fail("job_not_running", "language server stopped; inspect job_read(stream=stderr) and start a new protocol=lsp job")
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	length, size := -1, 0
	for {
		line, err := reader.ReadSlice('\n')
		if err != nil {
			return nil, err
		}
		size += len(line)
		if size > 8192 || !strings.HasSuffix(string(line), "\r\n") {
			return nil, errors.New("invalid or oversized LSP header")
		}
		if string(line) == "\r\n" {
			break
		}
		name, value, ok := strings.Cut(strings.TrimSuffix(string(line), "\r\n"), ":")
		if !ok {
			return nil, errors.New("expected Content-Length framing")
		}
		if strings.EqualFold(name, "Content-Length") {
			if length >= 0 {
				return nil, errors.New("duplicate Content-Length")
			}
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n < 1 || n > maxMessageBytes {
				return nil, errors.New("Content-Length must be 1–8388608 bytes")
			}
			length = n
		}
	}
	if length < 0 {
		return nil, errors.New("missing Content-Length")
	}
	data := make([]byte, length)
	_, err := io.ReadFull(reader, data)
	return data, err
}

func (c *Client) readLoop() {
	defer c.wg.Done()
	// Parent cancellation also interrupts a server stalled inside a frame.
	stop := context.AfterFunc(c.ctx, func() { _ = c.input.Close(); _ = c.output.Close() })
	defer stop()
	reader := bufio.NewReaderSize(c.output, 4096)
	for {
		data, err := readFrame(reader)
		if err != nil {
			code, message := "protocol_error", "invalid LSP framing; start with exec SERVER, emit no ordinary stdout, and restart the job: "+err.Error()
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || c.ctx.Err() != nil {
				code, message = "job_not_running", "language server output closed; inspect job_read(stream=stderr) and restart the job"
			}
			c.stop(fail(code, message))
			return
		}
		var message rpcMessage
		if err := json.Unmarshal(data, &message); err != nil || message.Version != "2.0" {
			c.stop(fail("protocol_error", "invalid language server JSON-RPC message; restart with a stdio LSP server"))
			return
		}
		if message.Method != "" {
			if len(message.ID) > 0 && string(message.ID) != "null" {
				ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
				err := c.answer(ctx, message)
				cancel()
				if err != nil {
					c.stop(err)
					return
				}
			}
			continue // Notifications are advisory; no editor state is restored from them.
		}
		if len(message.ID) == 0 || string(message.ID) == "null" {
			continue
		}
		c.mu.Lock()
		ch := c.pending[string(message.ID)]
		delete(c.pending, string(message.ID))
		c.mu.Unlock()
		if ch != nil {
			ch <- message
		} // Late/canceled and foreign IDs cannot complete another call.
	}
}

func (c *Client) send(ctx context.Context, message any) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(data) > maxMessageBytes {
		return fail("request_too_large", "LSP request exceeds 8 MiB; narrow the file or request")
	}
	select {
	case c.write <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return c.failure()
	}
	defer func() { <-c.write }()
	if err := ctx.Err(); err != nil {
		return err
	}
	// A blocked pipe write cannot safely resume after cancellation mid-frame.
	stop := context.AfterFunc(ctx, func() { c.stop(fail("job_not_running", "LSP write interrupted; restart the language server job")) })
	defer stop()
	frame := append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))), data...)
	for len(frame) > 0 {
		n, err := c.input.Write(frame)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.stop(fail("job_not_running", "language server stdin closed; inspect stderr and restart the job"))
			return c.failure()
		}
		if n == 0 {
			c.stop(fail("protocol_error", "language server pipe made no progress; restart the job"))
			return c.failure()
		}
		frame = frame[n:]
	}
	return nil
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	return c.send(ctx, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.next++
	id := c.next
	key := strconv.FormatInt(id, 10)
	reply := make(chan rpcMessage, 1)
	c.pending[key] = reply
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, key); c.mu.Unlock() }()
	if err := c.send(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case result := <-reply:
		if result.Error != nil {
			code := "server_error"
			if result.Error.Code == -32601 {
				code = "unsupported_operation"
			}
			if result.Error.Code == -32602 {
				code = "invalid_input"
			}
			if result.Error.Code == -32801 {
				code = "content_modified"
			}
			message := strings.Join(strings.Fields(render.Clean(result.Error.Message)), " ")
			if len(message) > 4096 {
				message = message[:4096]
				for !utf8.ValidString(message) {
					message = message[:len(message)-1]
				}
			}
			return nil, fail(code, fmt.Sprintf("LSP %s error %d: %s; check path/position and server configuration, then retry", method, result.Error.Code, message))
		}
		if len(result.Result) == 0 {
			return nil, fail("protocol_error", "LSP reply has no result or error; restart the server")
		}
		return result.Result, nil
	case <-ctx.Done():
		cancelCtx, cancel := context.WithTimeout(c.ctx, 250*time.Millisecond)
		_ = c.notify(cancelCtx, "$/cancelRequest", map[string]any{"id": id})
		cancel()
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, c.failure()
	}
}

func (c *Client) answer(ctx context.Context, request rpcMessage) error {
	var result any
	switch request.Method {
	case "workspace/configuration":
		var params struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil || len(params.Items) > 256 {
			return c.send(ctx, map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32602, "message": "configuration requires at most 256 items"}})
		}
		result = make([]any, len(params.Items)) // Null settings select each server's defaults.
	case "workspace/workspaceFolders":
		result = []any{map[string]string{"uri": fileURI(c.root), "name": c.root}}
	case "window/showMessageRequest":
		result = nil
	case "workspace/applyEdit":
		result = map[string]any{"applied": false, "failureReason": "TTC LSP queries are read-only; use edit or patch for file changes"}
	default:
		return c.send(ctx, map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": map[string]any{"code": -32601, "message": "TTC does not advertise this client capability"}})
	}
	return c.send(ctx, map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
}
