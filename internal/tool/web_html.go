package tool

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"ttc/internal/render"
)

// htmlMarkdown preserves common document semantics without executing scripts or
// fetching assets. Prefer a main/article region; navigation/form boilerplate is
// omitted. Unknown tags retain their text. Links resolve against the final URL.
func htmlMarkdown(ctx context.Context, source string, base *url.URL) (string, error) {
	root, err := html.Parse(contextReader{ctx, strings.NewReader(source)})
	if err != nil {
		return "", err
	}
	count := 0
	var main, article *html.Node
	var find func(*html.Node, int) error
	find = func(n *html.Node, depth int) error {
		count++
		if count&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if count > 200000 || depth > 128 {
			return fmt.Errorf("HTML structure exceeds conversion limit")
		}
		if n.Type == html.ElementNode {
			if n.Data == "main" && main == nil {
				main = n
			}
			if n.Data == "article" && article == nil {
				article = n
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if err := find(c, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := find(root, 0); err != nil {
		return "", err
	}
	if main != nil {
		root = main
	} else if article != nil {
		root = article
	}
	var convert func(*html.Node, bool, int) string
	ordinals := map[*html.Node]int{}
	var conversionErr error
	converted := 0
	convert = func(n *html.Node, pre bool, depth int) string {
		converted++
		if conversionErr != nil {
			return ""
		}
		if converted&255 == 0 {
			conversionErr = ctx.Err()
			if conversionErr != nil {
				return ""
			}
		}
		if n.Type == html.TextNode {
			if pre {
				return n.Data
			}
			text := strings.Join(strings.Fields(n.Data), " ")
			if text == "" {
				if n.Data != "" {
					return " "
				}
				return ""
			}
			if strings.ContainsAny(n.Data[:1], " \n\t\r") {
				text = " " + text
			}
			return text + spaceSuffix(n.Data)
		}
		if n.Type != html.ElementNode && n.Type != html.DocumentNode {
			return ""
		}
		switch n.Data {
		case "script", "style", "noscript", "nav", "form", "svg", "head":
			return ""
		}
		if n.Data == "pre" {
			var raw strings.Builder
			var collect func(*html.Node)
			collect = func(x *html.Node) {
				if x.Type == html.TextNode {
					raw.WriteString(x.Data)
				}
				for c := x.FirstChild; c != nil; c = c.NextSibling {
					collect(c)
				}
			}
			collect(n)
			lang := ""
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Data == "code" {
					for _, a := range c.Attr {
						if a.Key == "class" {
							for _, v := range strings.Fields(a.Val) {
								if strings.HasPrefix(v, "language-") {
									lang = strings.TrimPrefix(v, "language-")
								}
							}
						}
					}
				}
			}
			return "\n\n" + render.Fence(strings.TrimRight(raw.String(), "\n"), render.Clean(lang)) + "\n"
		}
		if n.Data == "table" {
			rows := [][]string{}
			tableBytes := int64(0)
			var visit func(*html.Node)
			visit = func(x *html.Node) {
				if conversionErr != nil {
					return
				}
				if x.Data == "tr" {
					cells := []string{}
					for c := x.FirstChild; c != nil; c = c.NextSibling {
						if c.Data == "td" || c.Data == "th" {
							value := strings.ReplaceAll(strings.Join(strings.Fields(convert(c, false, depth+1)), " "), "|", "\\|")
							tableBytes += int64(len(value)) + 6
							if tableBytes > webDownloadBytes {
								conversionErr = Fail("response_too_large", "converted table exceeds 4 MiB; retry with format=text or a narrower URL")
								return
							}
							cells = append(cells, value)
						}
					}
					if len(cells) > 0 {
						rows = append(rows, cells)
					}
					return
				}
				for c := x.FirstChild; c != nil; c = c.NextSibling {
					visit(c)
				}
			}
			visit(n)
			if conversionErr != nil {
				return ""
			}
			if len(rows) == 0 {
				return ""
			}
			columns := 0
			for _, r := range rows {
				columns = max(columns, len(r))
			}
			// Empty cells still cost output bytes. Bound rectangular padding
			// before allocating it, including the Markdown separator row.
			if tableBytes+int64(columns)*int64(len(rows))*3+int64(columns)*6+4 > webDownloadBytes {
				conversionErr = Fail("response_too_large", "converted table exceeds 4 MiB; retry with format=text or a narrower URL")
				return ""
			}
			var out strings.Builder
			out.WriteString("\n\n")
			for i, row := range rows {
				if i&127 == 0 {
					if conversionErr = ctx.Err(); conversionErr != nil {
						return ""
					}
				}
				for len(row) < columns {
					row = append(row, "")
				}
				out.WriteString("| " + strings.Join(row, " | ") + " |\n")
				if i == 0 {
					out.WriteString("|" + strings.Repeat(" --- |", columns) + "\n")
				}
			}
			return out.String() + "\n"
		}
		var out strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			part := convert(c, pre, depth+1)
			if out.Len()+len(part) > webDownloadBytes {
				conversionErr = Fail("response_too_large", "converted page exceeds 4 MiB; retry with format=text or a narrower URL")
				return ""
			}
			out.WriteString(part)
		}
		text := out.String()
		trim := strings.TrimSpace(text)
		switch n.Data {
		case "h1", "h2", "h3", "h4", "h5", "h6":
			return "\n\n" + strings.Repeat("#", int(n.Data[1]-'0')) + " " + trim + "\n\n"
		case "p", "div", "section", "article", "main", "ul", "ol":
			return "\n\n" + trim + "\n\n"
		case "li":
			prefix := "- "
			if n.Parent != nil && n.Parent.Data == "ol" {
				ordinals[n.Parent]++
				ordinal := ordinals[n.Parent]
				prefix = fmt.Sprintf("%d. ", ordinal)
			}
			return "\n" + prefix + strings.ReplaceAll(trim, "\n", "\n  ") + "\n"
		case "br":
			return "\n"
		case "hr":
			return "\n\n---\n\n"
		case "strong", "b":
			if trim != "" {
				return "**" + trim + "**"
			}
		case "em", "i":
			if trim != "" {
				return "*" + trim + "*"
			}
		case "code":
			fence := strings.SplitN(render.Fence(trim, ""), "\n", 2)[0]
			return fence + " " + trim + " " + fence
		case "blockquote":
			return "\n\n> " + strings.ReplaceAll(trim, "\n", "\n> ") + "\n\n"
		case "a":
			for _, a := range n.Attr {
				if a.Key == "href" {
					u, err := base.Parse(a.Val)
					if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil {
						return "[" + render.EscapeInline(trim) + "](" + strings.ReplaceAll(u.String(), ")", "%29") + ")"
					}
				}
			}
		case "img":
			for _, a := range n.Attr {
				if a.Key == "alt" && a.Val != "" {
					return "[image: " + render.EscapeInline(a.Val) + "]"
				}
			}
		}
		return text
	}
	text := convert(root, false, 0)
	if conversionErr != nil {
		return "", conversionErr
	}
	text = markdownSpacing(text)
	return strings.TrimSpace(text), nil
}
func spaceSuffix(text string) string {
	if text != "" && strings.ContainsAny(text[len(text)-1:], " \n\t\r") {
		return " "
	}
	return ""
}

func htmlText(source string) string {
	z := html.NewTokenizer(strings.NewReader(source))
	var out strings.Builder
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return strings.TrimSpace(out.String())
		case html.StartTagToken:
			t := z.Token()
			if t.Data == "script" || t.Data == "style" || t.Data == "noscript" {
				skip++
			}
			if skip == 0 && (t.Data == "p" || t.Data == "div" || t.Data == "br" || t.Data == "li" || len(t.Data) == 2 && t.Data[0] == 'h') {
				out.WriteString("\n")
			}
		case html.EndTagToken:
			t := z.Token()
			if (t.Data == "script" || t.Data == "style" || t.Data == "noscript") && skip > 0 {
				skip--
			}
		case html.TextToken:
			if skip == 0 {
				out.WriteString(z.Token().Data)
				out.WriteString(" ")
			}
		}
	}
}

// markdownSpacing collapses structural whitespace while leaving fenced bytes intact.
func markdownSpacing(text string) string {
	lines := strings.Split(text, "\n")
	out := lines[:0]
	fence := ""
	blank := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if fence != "" {
			out = append(out, line)
			if trim == fence {
				fence = ""
			}
			blank = false
			continue
		}
		if strings.HasPrefix(trim, "```") {
			n := 0
			for n < len(trim) && trim[n] == '`' {
				n++
			}
			fence = trim[:n]
		}
		if trim == "" && blank {
			continue
		}
		blank = trim == ""
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// contextReader lets the HTML parser stop between bounded input reads.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
