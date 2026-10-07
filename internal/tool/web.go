package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"ttc/internal/prompts"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
	"ttc/internal/history"
	"ttc/internal/render"
)

const webDownloadBytes = 4 << 20
const webCacheBytes = 16 << 20
const webCacheTTL = 15 * time.Minute

type webArgs struct {
	URL        string `json:"url,omitempty"`
	DocumentID string `json:"document_id,omitempty"`
	Format     string `json:"format,omitempty"`
	Offset     *int   `json:"offset,omitempty"` // Unicode characters, or matching-line cursor when pattern is set.
	Max        *int   `json:"max_chars,omitempty"`
	Pattern    string `json:"pattern,omitempty"` // RE2 search over retained lines.
	Context    *int   `json:"context_lines,omitempty"`
	Timeout    *int   `json:"timeout_ms,omitempty"`
}
type webDocument struct {
	id, url, contentType, format, text string
	expires                            time.Time
}
type webCache struct {
	mu    sync.Mutex
	docs  []webDocument
	bytes int
}

func (c *webCache) add(doc webDocument) (webDocument, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	doc.text = render.Clean(doc.text)
	doc.id, doc.expires = history.NewID("web"), time.Now().Add(webCacheTTL)
	size := len(doc.text)
	if size > webCacheBytes {
		return doc, errors.New(prompts.ToolWebRetainedLimit)
	}
	c.prune()
	for len(c.docs) > 0 && (c.bytes+size > webCacheBytes || len(c.docs) >= 8) {
		c.bytes -= len(c.docs[0].text)
		c.docs[0] = webDocument{}
		c.docs = c.docs[1:]
	}
	c.docs = append(c.docs, doc)
	c.bytes += size
	return doc, nil
}
func (c *webCache) prune() { // Caller owns mu; FIFO creation order is also expiry order.
	for len(c.docs) > 0 && time.Now().After(c.docs[0].expires) {
		c.bytes -= len(c.docs[0].text)
		c.docs[0] = webDocument{}
		c.docs = c.docs[1:]
	}
}
func (c *webCache) get(id string) (webDocument, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune()
	for _, doc := range c.docs {
		if doc.id == id {
			return doc, nil
		}
	}
	return webDocument{}, Fail("document_unavailable", prompts.ToolWebDocumentUnavailable)
}

func webURL(raw string) (*url.URL, error) {
	if len(raw) > 4096 {
		return nil, errors.New(prompts.ToolURLTooLarge)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return nil, errors.New(prompts.ToolURLRequired)
	}
	return u, nil
}

// AddWeb registers web search and retained-page fetch/search. Its bounded cache
// is shared by actors in this runtime; replacing the registry discards it.
func AddWeb(r *Registry, client *http.Client, config WebSearchConfig) {
	cache := &webCache{}
	fetchClient := *client
	fetchClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New(prompts.ToolWebRedirectLimit)
		}
		if _, err := webURL(req.URL.String()); err != nil {
			return err
		}
		if client.CheckRedirect != nil {
			return client.CheckRedirect(req, via)
		}
		return nil
	}
	Register(r, "web_fetch", prompts.ToolDescription("web_fetch"), map[string]any{"url": Property("string"), "document_id": Property("string"), "format": Property("string", "markdown", "text", "html"), "offset": Property("integer"), "max_chars": Property("integer"), "pattern": Property("string"), "context_lines": Property("integer"), "timeout_ms": Property("integer")}, nil, func(a webArgs) error {
		if (a.URL == "") == (a.DocumentID == "") {
			return errors.New(prompts.ToolWebSourceIdentifier)
		}
		if a.URL != "" {
			if _, err := webURL(a.URL); err != nil {
				return err
			}
		}
		if a.DocumentID != "" && a.Format != "" {
			return errors.New(prompts.ToolWebRetainedFormat)
		}
		if a.Format != "" && a.Format != "markdown" && a.Format != "text" && a.Format != "html" {
			return errors.New(prompts.ToolWebFormat)
		}
		if a.Offset != nil && *a.Offset < 0 {
			return errors.New(prompts.ToolWebOffset)
		}
		if err := rangeInt("max_chars", a.Max, 1, 64000); err != nil {
			return err
		}
		if err := rangeInt("timeout_ms", a.Timeout, 1, 120000); err != nil {
			return err
		}
		if err := rangeInt("context_lines", a.Context, 0, 10); err != nil {
			return err
		}
		if a.Context != nil && a.Pattern == "" {
			return errors.New(prompts.ToolWebContextPattern)
		}
		if len(a.Pattern) > 1024 {
			return errors.New(prompts.ToolWebPatternTooLarge)
		}
		if a.Pattern != "" {
			if _, err := regexp.Compile(a.Pattern); err != nil {
				return fmt.Errorf(prompts.ToolWebInvalidPattern, err)
			}
		}
		return nil
	}, func(ctx context.Context, x Execution, a webArgs) (any, error) {
		var doc webDocument
		var err error
		if a.DocumentID != "" {
			doc, err = cache.get(a.DocumentID)
		} else {
			doc, err = fetchWeb(ctx, &fetchClient, a)
			if err == nil {
				doc, err = cache.add(doc)
			}
		}
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, Fail("timeout", prompts.ToolWebTimeout)
			}
			return nil, err
		}
		return webPage(ctx, doc, a)
	})
	addWebSearch(r, client, cache, config)
}

func fetchWeb(ctx context.Context, client *http.Client, a webArgs) (webDocument, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(intDefault(a.Timeout, 30000))*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", a.URL, nil)
	if err != nil {
		return webDocument{}, err
	}
	req.Header.Set("Accept", "text/markdown, text/html;q=0.9, text/plain;q=0.8, application/json;q=0.7")
	resp, err := client.Do(req)
	if err != nil {
		return webDocument{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		hint := prompts.ToolWebHTTPSourceHint
		switch {
		case resp.StatusCode == 401 || resp.StatusCode == 403:
			hint = prompts.ToolWebHTTPAccessHint
		case resp.StatusCode == 429 || resp.StatusCode >= 500:
			hint = prompts.ToolWebHTTPRetryHint
		}
		return webDocument{}, fmt.Errorf(prompts.ToolWebHTTPFailed, resp.StatusCode, hint)
	}
	ct, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return webDocument{}, Fail("unsupported_content", prompts.ToolWebContentType)
	}
	if !strings.HasPrefix(ct, "text/") && ct != "application/json" && ct != "application/xml" && ct != "application/xhtml+xml" {
		return webDocument{}, Fail("unsupported_content", prompts.ToolWebTextResponse)
	}
	if resp.ContentLength > webDownloadBytes {
		return webDocument{}, Fail("response_too_large", prompts.ToolWebPageTooLarge)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, webDownloadBytes+1))
	if err != nil {
		return webDocument{}, err
	}
	if len(raw) > webDownloadBytes {
		return webDocument{}, Fail("response_too_large", prompts.ToolWebPageTooLarge)
	}
	text := string(raw)
	if ct == "text/html" || ct == "application/xhtml+xml" {
		reader, err := charset.NewReader(strings.NewReader(text), resp.Header.Get("Content-Type"))
		if err != nil {
			return webDocument{}, err
		}
		decoded, err := io.ReadAll(io.LimitReader(reader, webDownloadBytes+1))
		if err != nil {
			return webDocument{}, err
		}
		if len(decoded) > webDownloadBytes {
			return webDocument{}, Fail("response_too_large", prompts.ToolWebDecodedTooLarge)
		}
		text = string(decoded)
	}
	if !utf8.ValidString(text) {
		return webDocument{}, Fail("unsupported_content", prompts.ToolWebNotUTF8)
	}
	format := a.Format
	if format == "" {
		format = "markdown"
	}
	final := resp.Request.URL.String()
	if ct == "text/html" || ct == "application/xhtml+xml" {
		if format == "markdown" {
			text, err = htmlMarkdown(ctx, text, resp.Request.URL)
		} else if format == "text" {
			text = htmlText(text)
		}
		if err != nil {
			return webDocument{}, err
		}
	}
	return webDocument{url: final, contentType: ct, format: format, text: text}, nil
}

func webPage(ctx context.Context, doc webDocument, a webArgs) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	offset, limit := intDefault(a.Offset, 0), intDefault(a.Max, 4000)
	out := map[string]any{"document_id": doc.id, "url": doc.url, "content_type": doc.contentType, "format": doc.format, "untrusted": true, "offset": offset, "next_offset": nil, "truncated": false}
	if a.Pattern == "" {
		// Byte offsets into immutable UTF-8 avoid a second full-size rune allocation.
		begin, end, index := len(doc.text), len(doc.text), 0
		for pos := range doc.text {
			if pos&8191 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if index == offset {
				begin = pos
			}
			if index == offset+limit {
				end = pos
				break
			}
			index++
		}
		if end < begin {
			end = begin
		}
		out["content"] = doc.text[begin:end]
		out["cursor_unit"] = "unicode_characters"
		if end < len(doc.text) {
			out["next_offset"] = offset + limit
			out["truncated"] = true
		}
		return out, nil
	}
	re := regexp.MustCompile(a.Pattern) // Strict call validation already checked RE2 syntax.
	lines := strings.Split(doc.text, "\n")
	contextLines := intDefault(a.Context, 0)
	used, matched := 0, 0
	type match struct {
		Line      int    `json:"line"`
		Content   string `json:"content"`
		Truncated bool   `json:"truncated,omitempty"`
	}
	matches := []match{}
	for i, line := range lines {
		if i%128 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if !re.MatchString(line) {
			continue
		}
		if matched < offset {
			matched++
			continue
		}
		content := strings.Join(lines[max(0, i-contextLines):min(len(lines), i+contextLines+1)], "\n")
		n := utf8.RuneCountInString(content)
		if len(matches) > 0 && used+n > limit {
			out["next_offset"] = matched
			out["truncated"] = true
			break
		}
		cut := false
		if n > limit {
			content = string([]rune(content)[:limit])
			n = limit
			cut = true
		}
		matches = append(matches, match{i + 1, content, cut})
		used += n
		matched++
		if len(matches) >= 100 {
			out["next_offset"] = matched
			out["truncated"] = true
			break
		}
	}
	out["matches"], out["cursor_unit"] = matches, "matching_lines"
	return out, nil
}
