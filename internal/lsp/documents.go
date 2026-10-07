package lsp

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"ttc/internal/prompts"
)

type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}
type sourceRange struct {
	Start position `json:"start"`
	End   position `json:"end"`
}
type document struct {
	path, uri, language, text string
	version                   int
}

func fileURI(path string) string { return (&url.URL{Scheme: "file", Path: path}).String() }

func filePath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || u.Host != "" && u.Host != "localhost" || !filepath.IsAbs(u.Path) || u.RawQuery != "" || u.Fragment != "" {
		return "", fail("unsupported_content", prompts.LSPLocalFileURIRequired)
	}
	return filepath.Clean(u.Path), nil
}

func readText(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf(prompts.LSPReadFileFailed, path, err)
	}
	defer f.Close()
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxMessageBytes {
		return "", fail("unsupported_content", prompts.LSPRegularFileRequired)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxMessageBytes+1))
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	if len(data) > maxMessageBytes || !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return "", fail("unsupported_content", prompts.LSPInvalidFileText)
	}
	return string(data), nil
}

func language(path, supplied string) (string, error) {
	if strings.TrimSpace(supplied) != "" {
		return supplied, nil
	}
	name := map[string]string{".go": "go", ".py": "python", ".rs": "rust", ".c": "c", ".h": "c", ".cpp": "cpp", ".cc": "cpp", ".hpp": "cpp", ".js": "javascript", ".jsx": "javascriptreact", ".ts": "typescript", ".tsx": "typescriptreact", ".java": "java", ".json": "json", ".md": "markdown", ".tex": "latex", ".sh": "shellscript", ".yaml": "yaml", ".yml": "yaml", ".toml": "toml", ".lua": "lua", ".rb": "ruby", ".html": "html", ".css": "css"}[strings.ToLower(filepath.Ext(path))]
	if name == "" {
		return "", fail("invalid_input", prompts.LSPLanguageIDRequired)
	}
	return name, nil
}

func physicalLine(ctx context.Context, text string, line int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if line < 0 {
		return "", fail("invalid_position", prompts.LSPInvalidLine)
	}
	start := 0
	for i := 0; i < line; i++ {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		end := strings.IndexByte(text[start:], '\n')
		if end < 0 {
			return "", fail("invalid_position", fmt.Sprintf(prompts.LSPLineBeyondFile, line+1))
		}
		start += end + 1
	}
	end := strings.IndexByte(text[start:], '\n')
	if end < 0 {
		end = len(text) - start
	}
	return strings.TrimSuffix(text[start:start+end], "\r"), nil
}

func units(r rune, encoding string) int {
	switch encoding {
	case "utf-8":
		return utf8.RuneLen(r)
	case "utf-16":
		if r > 0xffff {
			return 2
		}
	}
	return 1
}

func wirePosition(ctx context.Context, text string, line, column int, encoding string) (position, error) {
	value, err := physicalLine(ctx, text, line-1)
	if err != nil {
		return position{}, err
	}
	if column < 1 {
		return position{}, fail("invalid_position", prompts.LSPInvalidColumn)
	}
	index, offset := 1, 0
	for _, char := range value {
		if index&1023 == 1 {
			if err := ctx.Err(); err != nil {
				return position{}, err
			}
		}
		if index == column {
			return position{line - 1, offset}, nil
		}
		index++
		offset += units(char, encoding)
	}
	if index == column {
		return position{line - 1, offset}, nil
	}
	return position{}, fail("invalid_position", fmt.Sprintf(prompts.LSPColumnBeyondLine, column, line, index))
}

func userPosition(ctx context.Context, text string, p position, encoding string) (int, int, error) {
	value, err := physicalLine(ctx, text, p.Line)
	if ctx.Err() != nil {
		return 0, 0, ctx.Err()
	}
	if err != nil || p.Character < 0 {
		return 0, 0, fail("invalid_server_result", prompts.LSPPositionOutsideFile)
	}
	column, offset := 1, 0
	for _, char := range value {
		if column&1023 == 1 {
			if err := ctx.Err(); err != nil {
				return 0, 0, err
			}
		}
		if offset == p.Character {
			return p.Line + 1, column, nil
		}
		next := offset + units(char, encoding)
		if next > p.Character {
			return 0, 0, fail("invalid_server_result", prompts.LSPPositionInsideCharacter)
		}
		offset = next
		column++
	}
	// The protocol clamps an overlong character offset to the physical line end.
	return p.Line + 1, column, nil
}

func (c *Client) syncDocument(ctx context.Context, path, id string) (string, error) {
	text, err := readText(ctx, path)
	if err != nil {
		return "", err
	}
	index := -1
	for i, doc := range c.documents {
		if doc.path == path {
			index = i
			break
		}
	}
	if index >= 0 {
		retained := len(text) - len(c.documents[index].text)
		for _, doc := range c.documents {
			retained += len(doc.text)
		}
		for retained > 16<<20 {
			evict := 0
			if index == 0 {
				evict = 1
			}
			doc := c.documents[evict]
			if c.openClose {
				if err := c.notify(ctx, "textDocument/didClose", map[string]any{"textDocument": map[string]string{"uri": doc.uri}}); err != nil {
					return "", err
				}
			}
			retained -= len(doc.text)
			copy(c.documents[evict:], c.documents[evict+1:])
			c.documents[len(c.documents)-1] = document{}
			c.documents = c.documents[:len(c.documents)-1]
			if evict < index {
				index--
			}
		}
		doc := &c.documents[index]
		if doc.language != id {
			return "", fail("invalid_input", prompts.LSPLanguageIDChanged)
		}
		if doc.text == text {
			return text, nil
		}
		if c.change == 0 {
			return "", fail("unsupported_operation", prompts.LSPTextChangesUnsupported)
		}
		version := doc.version + 1
		change := map[string]any{"text": text}
		if c.change == 2 {
			last := strings.Count(doc.text, "\n") + 1
			lastText, err := physicalLine(ctx, doc.text, last-1)
			if err != nil {
				return "", err
			}
			end, err := wirePosition(ctx, doc.text, last, utf8.RuneCountInString(lastText)+1, c.encoding)
			if err != nil {
				return "", err
			}
			change["range"] = sourceRange{Start: position{}, End: end}
		}
		if err := c.notify(ctx, "textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": doc.uri, "version": version}, "contentChanges": []any{change}}); err != nil {
			return "", err
		}
		doc.text, doc.version = text, version
		return text, nil
	}
	retained := len(text)
	for _, doc := range c.documents {
		retained += len(doc.text)
	}
	for len(c.documents) > 0 && (len(c.documents) >= 8 || retained > 16<<20) {
		doc := c.documents[0]
		if c.openClose {
			if err := c.notify(ctx, "textDocument/didClose", map[string]any{"textDocument": map[string]string{"uri": doc.uri}}); err != nil {
				return "", err
			}
		}
		retained -= len(doc.text)
		c.documents[0] = document{}
		c.documents = c.documents[1:]
	}
	doc := document{path: path, uri: fileURI(path), language: id, text: text, version: 1}
	if c.openClose {
		if err := c.notify(ctx, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": doc.uri, "languageId": id, "version": 1, "text": text}}); err != nil {
			return "", err
		}
	}
	c.documents = append(c.documents, doc)
	return text, nil
}
