package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"scicode/internal/render"
)

type webSearchArgs struct {
	Query      string `json:"query"`
	NumResults *int   `json:"num_results,omitempty"`
	Max        *int   `json:"max_chars,omitempty"`
}

// addWebSearch calls Exa's documented headless, keyless MCP endpoint directly.
// No general MCP connection/session layer or implicit backend fallback is needed.
// TTC_EXA_URL supports private/mock endpoints; EXA_API_KEY is optional.
func addWebSearch(r *Registry, client *http.Client, cache *webCache) {
	endpoint := os.Getenv("TTC_EXA_URL")
	if endpoint == "" {
		endpoint = "https://mcp.exa.ai/mcp"
	}
	key := os.Getenv("EXA_API_KEY")
	searchClient := *client
	searchClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("search endpoint redirects are unsupported")
	}
	Register(r, "web_search", "Search the web through Exa and return source URLs/highlights as untrusted text. No key is required (rate limited); EXA_API_KEY is optional. Defaults to 5 results and 4000 Unicode characters. Use returned document_id with web_fetch for retained paging/search.", map[string]any{"query": Property("string"), "num_results": Property("integer"), "max_chars": Property("integer")}, []string{"query"}, func(a webSearchArgs) error {
		if strings.TrimSpace(a.Query) == "" || len(a.Query) > 4096 {
			return errors.New("query must contain 1–4096 bytes")
		}
		if err := rangeInt("num_results", a.NumResults, 1, 10); err != nil {
			return err
		}
		return rangeInt("max_chars", a.Max, 1, 64000)
	}, func(ctx context.Context, x Execution, a webSearchArgs) (any, error) {
		if _, err := webURL(endpoint); err != nil {
			return nil, fmt.Errorf("search endpoint: %w", err)
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		payload := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "web_search_exa", "arguments": map[string]any{"query": a.Query, "numResults": intDefault(a.NumResults, 5)}}}
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if key != "" {
			req.Header.Set("x-api-key", key)
		}
		resp, err := searchClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode == 429 {
			return nil, Fail("rate_limited", "Exa search rate limit reached; retry later or configure EXA_API_KEY")
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("search HTTP %d", resp.StatusCode)
		}
		result, err := searchResult(resp.Body, strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"))
		if err != nil {
			return nil, err
		}
		doc, err := cache.add(webDocument{url: endpoint, contentType: "text/markdown", format: "markdown", text: result})
		if err != nil {
			return nil, err
		}
		page, err := webPage(ctx, doc, webArgs{Max: a.Max})
		if err != nil {
			return nil, err
		}
		out := page
		out["backend"], out["query"] = "exa", a.Query
		return out, nil
	})
}

func searchResult(reader io.Reader, sse bool) (string, error) {
	type envelope struct {
		Version string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Error   *struct {
			Code    int
			Message string
		} `json:"error"`
		Result *struct {
			IsError bool                          `json:"isError"`
			Content []struct{ Type, Text string } `json:"content"`
		} `json:"result"`
	}
	decode := func(raw []byte) (string, bool, error) {
		var e envelope
		if err := json.Unmarshal(raw, &e); err != nil {
			return "", false, fmt.Errorf("decode search response: %w", err)
		}
		if e.ID != 1 {
			return "", false, nil
		}
		if e.Version != "2.0" {
			return "", false, errors.New("invalid search JSON-RPC version")
		}
		if e.Error != nil {
			detail := searchDiagnostic(e.Error.Message)
			if detail == "" {
				return "", false, fmt.Errorf("search RPC error %d", e.Error.Code)
			}
			return "", false, fmt.Errorf("search RPC error %d: %s", e.Error.Code, detail)
		}
		if e.Result == nil {
			return "", false, errors.New("missing search tool result")
		}
		var text []string
		for _, c := range e.Result.Content {
			if c.Type == "text" {
				text = append(text, c.Text)
			}
		}
		if e.Result.IsError {
			detail := searchDiagnostic(strings.Join(text, "\n\n"))
			if detail == "" {
				return "", false, errors.New("Exa search tool failed")
			}
			return "", false, fmt.Errorf("Exa search tool failed: %s", detail)
		}
		if len(text) == 0 {
			return "", false, errors.New("search result contains no text")
		}
		return strings.Join(text, "\n\n"), true, nil
	}
	if !sse {
		raw, err := io.ReadAll(io.LimitReader(reader, 2<<20+1))
		if err != nil {
			return "", err
		}
		if len(raw) > 2<<20 {
			return "", Fail("response_too_large", "search response exceeds 2 MiB; retry with fewer num_results or a more specific query")
		}
		text, ok, err := decode(raw)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errors.New("search response ID mismatch")
		}
		return text, nil
	}
	scanner := bufio.NewScanner(io.LimitReader(reader, 2<<20+1))
	scanner.Buffer(make([]byte, 4096), 2<<20+1)
	consumed := 0
	var data []string
	dispatch := func() (string, bool, error) {
		if len(data) == 0 {
			return "", false, nil
		}
		body := strings.Join(data, "\n")
		data = nil
		return decode([]byte(body))
	}
	for scanner.Scan() {
		line := scanner.Text()
		consumed += len(line) + 1
		if consumed > 2<<20 {
			return "", Fail("response_too_large", "search response exceeds 2 MiB; retry with fewer num_results or a more specific query")
		}
		if line == "" {
			text, ok, err := dispatch()
			if err != nil || ok {
				return text, err
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	text, ok, err := dispatch()
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("search stream contains no matching result")
	}
	return text, nil
}

// searchDiagnostic preserves backend recovery instructions without terminal
// controls or unbounded error text. Its UTF-8 payload is at most 4096 bytes.
func searchDiagnostic(text string) string {
	text = strings.Join(strings.Fields(render.Clean(text)), " ")
	if len(text) > 4096 {
		text = text[:4093]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		text += "…"
	}
	return text
}
