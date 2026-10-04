package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"ttc/internal/render"
)

// Location uses absolute disk paths and 1-based Unicode code-point coordinates.
// End is exclusive, matching the language server's source range.
type Location struct {
	Path        string `json:"path"`
	StartLine   int    `json:"start_line"`
	StartColumn int    `json:"start_column"`
	EndLine     int    `json:"end_line"`
	EndColumn   int    `json:"end_column"`
}

// Symbol adds the protocol's human-readable kind and optional parent container.
type Symbol struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Location
	Container string `json:"container_name,omitempty"`
}

type resultSources struct {
	texts map[string]string
	bytes int
}

func (c *Client) location(ctx context.Context, uri string, span sourceRange, sources *resultSources) (Location, error) {
	if err := ctx.Err(); err != nil {
		return Location{}, err
	}
	path, err := filePath(uri)
	if err != nil {
		return Location{}, err
	}
	text, found := sources.texts[path]
	for _, doc := range c.documents {
		if doc.path == path {
			text, found = doc.text, true
			break
		}
	}
	if !found {
		text, err = readText(ctx, path)
		if err != nil {
			return Location{}, err
		}
	}
	if _, exists := sources.texts[path]; !exists {
		if sources.bytes+len(text) > 16<<20 {
			return Location{}, fail("result_too_large", "LSP result source files exceed 16 MiB; reduce limit and page with offset")
		}
		sources.texts[path] = text
		sources.bytes += len(text)
	}
	line, column, err := userPosition(ctx, text, span.Start, c.encoding)
	if err != nil {
		return Location{}, err
	}
	endLine, endColumn, err := userPosition(ctx, text, span.End, c.encoding)
	if err != nil {
		return Location{}, err
	}
	if endLine < line || endLine == line && endColumn < column {
		return Location{}, fail("invalid_server_result", "LSP returned a reversed source range")
	}
	return Location{path, line, column, endLine, endColumn}, nil
}

func resultArray(raw json.RawMessage, single bool) ([]json.RawMessage, error) {
	if string(raw) == "null" {
		return []json.RawMessage{}, nil
	}
	if single && strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return []json.RawMessage{raw}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fail("invalid_server_result", "LSP result must be a list of source locations/symbols")
	}
	return items, nil
}

func pagination(offset, limit, total int) (int, int, any) {
	start := min(offset, total)
	end := start + min(limit, total-start)
	var next any
	if end < total {
		next = end
	}
	return start, end, next
}

func (c *Client) locations(ctx context.Context, raw json.RawMessage, offset, limit int) (map[string]any, error) {
	items, err := resultArray(raw, true)
	if err != nil {
		return nil, err
	}
	start, end, next := pagination(offset, limit, len(items))
	locations := make([]Location, 0, end-start)
	sources := resultSources{texts: map[string]string{}}
	for _, item := range items[start:end] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var value struct {
			URI         string       `json:"uri"`
			Range       *sourceRange `json:"range"`
			TargetURI   string       `json:"targetUri"`
			TargetRange *sourceRange `json:"targetRange"`
			Selection   *sourceRange `json:"targetSelectionRange"`
		}
		if err := json.Unmarshal(item, &value); err != nil {
			return nil, fail("invalid_server_result", "invalid LSP location")
		}
		uri, span := value.URI, value.Range
		if value.TargetURI != "" {
			uri, span = value.TargetURI, value.Selection
			if span == nil {
				span = value.TargetRange
			}
		}
		if span == nil {
			return nil, fail("invalid_server_result", "LSP location has no source range")
		}
		location, err := c.location(ctx, uri, *span, &sources)
		if err != nil {
			return nil, err
		}
		locations = append(locations, location)
	}
	return map[string]any{"kind": "locations", "locations": locations, "next_offset": next, "truncated": next != nil}, nil
}

func hover(raw json.RawMessage) (map[string]any, error) {
	var result struct {
		Contents json.RawMessage `json:"contents"`
	}
	if string(raw) != "null" {
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fail("invalid_server_result", "invalid LSP hover result")
		}
	}
	if len(result.Contents) == 0 || string(result.Contents) == "null" {
		return map[string]any{"kind": "hover", "markdown": nil}, nil
	}
	parts, err := resultArray(result.Contents, true)
	if strings.HasPrefix(strings.TrimSpace(string(result.Contents)), "\"") {
		parts = []json.RawMessage{result.Contents}
		err = nil
	}
	if err != nil {
		return nil, err
	}
	var texts []string
	for _, part := range parts {
		var text string
		if json.Unmarshal(part, &text) == nil {
			texts = append(texts, render.Clean(text))
			continue
		}
		var value struct{ Kind, Language, Value string }
		if err := json.Unmarshal(part, &value); err != nil {
			return nil, fail("invalid_server_result", "invalid LSP hover contents")
		}
		if value.Language != "" {
			text = render.Fence(value.Value, value.Language)
		} else if value.Kind == "plaintext" {
			text = render.Fence(value.Value, "text")
		} else {
			text = render.Clean(value.Value)
		}
		texts = append(texts, text)
	}
	return map[string]any{"kind": "hover", "markdown": strings.Join(texts, "\n\n")}, nil
}

type wireSymbol struct {
	Name      string       `json:"name"`
	Kind      int          `json:"kind"`
	Container string       `json:"containerName"`
	Range     *sourceRange `json:"range"`
	Location  *struct {
		URI   string       `json:"uri"`
		Range *sourceRange `json:"range"`
	} `json:"location"`
	Children []json.RawMessage `json:"children"`
}

func symbolKind(kind int) string {
	names := []string{"file", "module", "namespace", "package", "class", "method", "property", "field", "constructor", "enum", "interface", "function", "variable", "constant", "string", "number", "boolean", "array", "object", "key", "null", "enum_member", "struct", "event", "operator", "type_parameter"}
	if kind < 1 || kind > len(names) {
		return fmt.Sprintf("unknown_%d", kind)
	}
	return names[kind-1]
}

func (c *Client) symbols(ctx context.Context, raw json.RawMessage, path string, offset, limit int) (map[string]any, error) {
	items, err := resultArray(raw, false)
	if err != nil {
		return nil, err
	}
	type pending struct {
		raw       json.RawMessage
		container string
		depth     int
	}
	stack := make([]pending, 0, len(items))
	for i := len(items) - 1; i >= 0; i-- {
		stack = append(stack, pending{raw: items[i]})
	}
	symbols := make([]Symbol, 0, limit)
	sources := resultSources{texts: map[string]string{}}
	seen := 0
	var next any
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if item.depth > 64 {
			return nil, fail("result_too_large", "LSP symbol hierarchy exceeds 64 levels; narrow the workspace query or use another server")
		}
		var value wireSymbol
		if err := json.Unmarshal(item.raw, &value); err != nil {
			return nil, fail("invalid_server_result", "invalid LSP symbol")
		}
		container := value.Container
		if container == "" {
			container = item.container
		}
		for i := len(value.Children) - 1; i >= 0; i-- {
			stack = append(stack, pending{raw: value.Children[i], container: value.Name, depth: item.depth + 1})
		}
		if seen < offset {
			seen++
			continue
		}
		if len(symbols) >= limit {
			next = seen
			break
		}
		uri, span := fileURI(path), value.Range
		if value.Location != nil {
			uri, span = value.Location.URI, value.Location.Range
		}
		var workspaceOptions struct {
			Resolve bool `json:"resolveProvider"`
		}
		_ = json.Unmarshal(c.capabilities["workspaceSymbolProvider"], &workspaceOptions)
		if span == nil && path == "" && workspaceOptions.Resolve {
			// Servers may return unresolved workspace symbols under LSP 3.17.
			resolved, err := c.call(ctx, "workspaceSymbol/resolve", item.raw)
			if err != nil {
				return nil, err
			}
			if err = json.Unmarshal(resolved, &value); err != nil {
				return nil, fail("invalid_server_result", "invalid resolved workspace symbol")
			}
			if value.Location != nil {
				uri, span = value.Location.URI, value.Location.Range
			}
		}
		if span == nil {
			return nil, fail("invalid_server_result", "LSP symbol has no source location; use a server supporting source ranges")
		}
		location, err := c.location(ctx, uri, *span, &sources)
		if err != nil {
			return nil, err
		}
		symbols = append(symbols, Symbol{Name: value.Name, Kind: symbolKind(value.Kind), Location: location, Container: container})
		seen++
	}
	return map[string]any{"kind": "symbols", "symbols": symbols, "next_offset": next, "truncated": next != nil}, nil
}
