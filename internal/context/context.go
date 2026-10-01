// Package context builds bounded model context and immutable submitted attachments.
package context

import (
	stdcontext "context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"scicode/internal/provider"
	"sort"
	"strings"
	"unicode/utf8"
)

// Estimate conservatively approximates text/wire tokens from UTF-8 bytes, not reported usage.
func Estimate(text string) int { return (len(text) + 2) / 3 }

// Tokens includes messages, calls, and provider-native replay payloads; image estimates reserve 4096 tokens each.
func Tokens(messages []provider.Message) int {
	n := 0
	for _, m := range messages {
		if m.State != nil {
			for _, item := range m.State.Items {
				n += Estimate(string(item))
			}
			continue
		}
		n += 16 + Estimate(m.Content)
		for _, c := range m.Calls {
			n += 16 + Estimate(string(c.Arguments)) + Estimate(c.Name)
		}
		n += 4096 * len(m.Images)
	}
	return n
}

// Fits reserves next-turn headroom when requested.
func Fits(selection provider.Selection, system string, tools []provider.ToolDefinition, messages []provider.Message, headroom bool) bool {
	n := Estimate(system) + Tokens(messages)
	for _, t := range tools {
		n += Estimate(t.Description) + Estimate(string(t.Parameters)) + 16
	}
	b := selection.Model.Budget
	n += b.OutputAllowance + b.EstimationMargin
	if headroom {
		n += b.NextTurnInputReserve
	}
	return n < b.ContextLimit
}

// Retention describes the suffix to copy after a summary. User is -1 for whole
// turns, or the initiating human message index to copy before a partial turn.
type Retention struct {
	Start int // First retained suffix index; len(messages) retains only User.
	User  int
}

// Retain keeps whole recent user turns within target tokens. When the newest
// turn exceeds target, it preserves its initiating user message and cuts only
// at completed tool cycles. The last two assistant messages and their tool
// results are mandatory, even above the desired target; callers check capacity.
// Unresolved calls fail; mandatory user text is never truncated to fit target.
func Retain(messages []provider.Message, target int) (Retention, error) {
	fail := func(reason string) (Retention, error) { return Retention{}, errors.New(reason) }
	if len(messages) == 0 {
		return fail("nothing to compact")
	}
	suffix := make([]int, len(messages)+1)
	for i := len(messages) - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1] + Tokens(messages[i:i+1])
	}
	pending := map[string]bool{}
	safe := make([]bool, len(messages)+1)
	safe[0] = true
	for i, message := range messages {
		for _, call := range message.Calls {
			if call.ID == "" || pending[call.ID] {
				return fail("invalid tool call sequence prevents compaction")
			}
			pending[call.ID] = true
		}
		if message.Role == "tool" {
			if !pending[message.CallID] {
				return fail("unpaired tool result prevents compaction")
			}
			delete(pending, message.CallID)
		}
		safe[i+1] = len(pending) == 0
	}
	if len(pending) != 0 {
		return fail("unresolved tool calls prevent compaction")
	}
	cut := len(messages)
	latestUser := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" || messages[i].Runtime {
			continue
		}
		if latestUser < 0 {
			latestUser = i
		}
		if suffix[i] > target {
			break
		}
		cut = i
	}
	if cut == 0 {
		return fail("nothing to compact")
	}
	if cut == len(messages) {
		if latestUser < 0 {
			return fail("no initiating user message to retain")
		}
		userTokens := suffix[latestUser] - suffix[latestUser+1]
		mandatory := len(messages)
		assistants := 0
		for i := len(messages) - 1; i > latestUser; i-- {
			if messages[i].Role == "assistant" {
				mandatory = i
				assistants++
				if assistants == 2 {
					break
				}
			}
		}
		// No completed older model work exists to summarize in this turn.
		if assistants == 0 || mandatory <= latestUser+1 || !safe[mandatory] {
			return fail("no completed older model cycle can be compacted")
		}
		for i := latestUser + 2; i <= mandatory; i++ {
			if safe[i] && suffix[i]+userTokens <= target {
				return Retention{Start: i, User: latestUser}, nil
			}
		}
		return Retention{Start: mandatory, User: latestUser}, nil
	}
	if !safe[cut] {
		return fail("no safe user-turn compaction cut")
	}
	return Retention{Start: cut, User: -1}, nil
}

// Attachment is a snapshot made before queuing user input; bytes do not change on disk edits.
type Attachment struct {
	Path      string          `json:"path"`
	Kind      string          `json:"kind"`
	Text      string          `json:"text,omitempty"`
	Image     *provider.Image `json:"image,omitempty"`
	Truncated bool            `json:"truncated"`
}

// Snapshot rejects unsupported content and never follows directory symlinks.
func Snapshot(ctx stdcontext.Context, path string, images bool) (Attachment, error) {
	if err := ctx.Err(); err != nil {
		return Attachment{}, err
	}
	path, e := filepath.Abs(path)
	if e != nil {
		return Attachment{}, e
	}
	st, e := os.Stat(path)
	if e != nil {
		return Attachment{}, e
	}
	a := Attachment{Path: path}
	if st.IsDir() {
		a.Kind = "directory"
		paths := []string{}
		var walk func(string) error
		walk = func(dir string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			f, err := os.Open(dir)
			if err != nil {
				return err
			}
			defer f.Close()
			for len(paths) < 500 {
				if err := ctx.Err(); err != nil {
					return err
				}
				entries, err := f.ReadDir(min(128, 500-len(paths)))
				for _, d := range entries {
					p := filepath.Join(dir, d.Name())
					rel, err := filepath.Rel(path, p)
					if err != nil {
						return err
					}
					paths = append(paths, rel)
					if d.IsDir() && len(paths) < 500 {
						if err := walk(p); err != nil {
							return err
						}
					}
					if len(paths) >= 500 {
						a.Truncated = true
						return nil
					}
				}
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
			}
			a.Truncated = true
			return nil
		}
		e = walk(path)
		if e != nil {
			return a, e
		}
		sort.Strings(paths)
		a.Text = strings.Join(paths, "\n")
		return a, nil
	}
	if !st.Mode().IsRegular() {
		return a, errors.New("attachment must be a regular file or directory")
	}
	if st.Size() > 8<<20 {
		return a, errors.New("attachment exceeds 8 MiB")
	}
	f, e := os.Open(path)
	if e != nil {
		return a, e
	}
	defer f.Close()
	stop := stdcontext.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()
	b, e := io.ReadAll(io.LimitReader(f, 8<<20+1))
	if err := ctx.Err(); err != nil {
		return a, err
	}
	if len(b) > 8<<20 {
		return a, errors.New("attachment exceeds 8 MiB")
	}
	if e != nil {
		return a, e
	}
	mime := ""
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		mime = "image/png"
	case ".jpg", ".jpeg":
		mime = "image/jpeg"
	case ".gif":
		mime = "image/gif"
	}
	if mime != "" {
		if !images {
			return a, errors.New("selected model does not support image attachments")
		}
		a.Kind = "image"
		a.Image = &provider.Image{Path: path, DataURL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)}
		return a, nil
	}
	if !utf8.Valid(b) || strings.ContainsRune(string(b), 0) {
		return a, errors.New("unsupported attachment content")
	}
	a.Kind = "text"
	if len(b) > 32768 {
		b = b[:32768]
		for !utf8.Valid(b) {
			b = b[:len(b)-1]
		}
		a.Truncated = true
	}
	a.Text = string(b)
	return a, nil
}

// Message combines submitted text and immutable attachments without silently dropping any.
func Message(text string, attachments []Attachment) provider.Message {
	m := provider.Message{Role: "user", Content: text}
	for _, a := range attachments {
		if a.Image != nil {
			m.Images = append(m.Images, *a.Image)
			m.Content += "\nImage attachment: " + a.Path
			continue
		}
		m.Content += fmt.Sprintf("\n\nAttachment (%s): %s\n%s", a.Kind, a.Path, a.Text)
		if a.Truncated {
			m.Content += "\n[attachment truncated]"
		}
	}
	return m
}
