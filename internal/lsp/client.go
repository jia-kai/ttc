package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Query selects one read-only operation. Paths are absolute or relative to the
// server root. Lines/columns use 1-based Unicode code points; offsets are 0-based.
// Callers validate operation-specific fields and supply a positive limit <=500.
type Query struct {
	Operation, Path, LanguageID, Text string
	Line, Column, Offset, Limit       int
}

func (c *Client) initialize(ctx context.Context) error {
	if c.initialized {
		return nil
	}
	if c.attempted {
		return fail("initialization_failed", "language server initialization failed or was interrupted; stop and restart this job")
	}
	c.attempted = true
	kinds := make([]int, 26)
	for i := range kinds {
		kinds[i] = i + 1
	}
	symbol := map[string]any{"symbolKind": map[string]any{"valueSet": kinds}, "resolveSupport": map[string]any{"properties": []string{"location.range"}}}
	params := map[string]any{"processId": os.Getpid(), "clientInfo": map[string]string{"name": "TTC"}, "rootUri": fileURI(c.root), "workspaceFolders": []any{map[string]string{"uri": fileURI(c.root), "name": filepath.Base(c.root)}}, "capabilities": map[string]any{
		"general":      map[string]any{"positionEncodings": []string{"utf-8", "utf-16", "utf-32"}},
		"workspace":    map[string]any{"configuration": true, "workspaceFolders": true, "applyEdit": false, "symbol": symbol},
		"textDocument": map[string]any{"hover": map[string]any{"contentFormat": []string{"markdown", "plaintext"}}, "definition": map[string]any{"linkSupport": true}, "documentSymbol": map[string]any{"hierarchicalDocumentSymbolSupport": true}},
	}}
	raw, err := c.call(ctx, "initialize", params)
	if err != nil {
		return err
	}
	var result struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Capabilities == nil {
		return fail("protocol_error", "LSP initialize returned no capabilities; restart a correctly configured language server")
	}
	c.capabilities = result.Capabilities
	if value := result.Capabilities["positionEncoding"]; len(value) > 0 {
		if err := json.Unmarshal(value, &c.encoding); err != nil {
			return fail("protocol_error", "invalid LSP positionEncoding")
		}
	}
	if c.encoding != "utf-8" && c.encoding != "utf-16" && c.encoding != "utf-32" {
		return fail("unsupported_encoding", "language server chose unsupported positionEncoding; use UTF-8, UTF-16 or UTF-32")
	}
	if value := result.Capabilities["textDocumentSync"]; len(value) > 0 && string(value) != "null" {
		if err := json.Unmarshal(value, &c.change); err == nil {
			c.openClose = c.change != 0
		} else {
			var sync struct {
				OpenClose bool `json:"openClose"`
				Change    int  `json:"change"`
			}
			if err := json.Unmarshal(value, &sync); err != nil {
				return fail("protocol_error", "invalid LSP textDocumentSync")
			}
			c.openClose, c.change = sync.OpenClose, sync.Change
		}
		if c.change < 0 || c.change > 2 {
			return fail("protocol_error", "invalid LSP textDocumentSync change kind")
		}
	}
	if err := c.notify(ctx, "initialized", map[string]any{}); err != nil {
		return err
	}
	c.initialized = true
	return nil
}

func (c *Client) supports(name string) bool {
	value := strings.TrimSpace(string(c.capabilities[name]))
	return value != "" && value != "false" && value != "null"
}

// Query initializes the server once, synchronizes the requested file, and
// returns portable results. The caller's context bounds queueing and all RPCs.
func (c *Client) Query(ctx context.Context, q Query) (map[string]any, error) {
	if q.Offset < 0 || q.Limit < 1 || q.Limit > 500 {
		return nil, fail("invalid_input", "offset must be nonnegative and limit must be 1–500")
	}
	methods := map[string][2]string{"definition": {"textDocument/definition", "definitionProvider"}, "references": {"textDocument/references", "referencesProvider"}, "hover": {"textDocument/hover", "hoverProvider"}, "document_symbols": {"textDocument/documentSymbol", "documentSymbolProvider"}, "workspace_symbols": {"workspace/symbol", "workspaceSymbolProvider"}}
	method, ok := methods[q.Operation]
	if !ok {
		return nil, fail("invalid_input", "operation must be definition, references, hover, document_symbols or workspace_symbols")
	}
	select {
	case c.query <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, c.failure()
	}
	defer func() { <-c.query }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	if !c.supports(method[1]) {
		return nil, fail("unsupported_operation", fmt.Sprintf("language server does not advertise %s; choose another operation or server", q.Operation))
	}
	params := map[string]any{}
	path := ""
	if q.Operation == "workspace_symbols" {
		params["query"] = q.Text
	} else {
		if q.Path == "" {
			return nil, fail("invalid_input", "path is required for this operation")
		}
		path = q.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(c.root, path)
		}
		path = filepath.Clean(path)
		id, err := language(path, q.LanguageID)
		if err != nil {
			return nil, err
		}
		text, err := c.syncDocument(ctx, path, id)
		if err != nil {
			return nil, err
		}
		params["textDocument"] = map[string]string{"uri": fileURI(path)}
		if q.Operation != "document_symbols" {
			pos, err := wirePosition(ctx, text, q.Line, q.Column, c.encoding)
			if err != nil {
				return nil, err
			}
			params["position"] = pos
		}
		if q.Operation == "references" {
			params["context"] = map[string]bool{"includeDeclaration": true}
		}
	}
	raw, err := c.call(ctx, method[0], params)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	switch q.Operation {
	case "definition", "references":
		result, err = c.locations(ctx, raw, q.Offset, q.Limit)
	case "hover":
		result, err = hover(raw)
	default:
		result, err = c.symbols(ctx, raw, path, q.Offset, q.Limit)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return result, err
}
