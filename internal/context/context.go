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
	"sort"
	"strings"
	"syscall"
	"ttc/internal/provider"
	"unicode/utf8"
)

// Estimate conservatively approximates text/wire tokens from UTF-8 bytes, not reported usage.
func Estimate(text string) int { return (len(text) + 2) / 3 }

// Tokens includes messages, calls and adapter-estimated native replay occupancy.
// Unannotated replay uses a conservative transport estimate; binary files use
// format-dependent reserves, not their base64 transport size.
func Tokens(messages []provider.Message) int {
	n := 0
	for _, m := range messages {
		if m.State != nil {
			n += provider.ReplayTokens(m.State)
			continue
		}
		n += 16 + Estimate(m.Content)
		for _, c := range m.Calls {
			n += 16 + Estimate(string(c.Arguments)) + Estimate(c.Name)
		}
		for _, file := range m.Files {
			n += file.EstimatedTokens()
		}
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

// Retention describes independently selected human inputs and the recent
// model/tool tail to copy after a summary, not complete user/model turns.
type Retention struct {
	Start  int   // First retained suffix index; len(messages) retains only Inputs.
	Inputs []int // Sorted human-message indices before Start: at most two non-steers and two steers.
}

// Retain targets the tokens in the latest two assistant/tool cycles after the
// latest human instruction, clamped to [minTokens, maxTokens]. It keeps the
// longest suffix within that target starting at a complete cycle boundary;
// oversized cycles enter the summary rather than exceeding maxTokens, except
// the latest cycle with binary tool results not yet followed by an assistant
// response. That cycle must remain intact, even above the tail target; callers
// reject compaction if it cannot fit the complete request. The last
// two normal/queued inputs combined and last two steers are retained separately,
// outside the tail budget; task/btw inputs count with normal/queued inputs.
// Runtime notices are not human inputs. Unresolved calls fail, and callers check
// capacity/headroom for the complete replacement input.
func Retain(messages []provider.Message, minTokens, maxTokens int) (Retention, error) {
	fail := func(reason string) (Retention, error) { return Retention{}, errors.New(reason) }
	if minTokens < 0 || maxTokens <= 0 || minTokens > maxTokens {
		return fail("invalid recent-cycle token bounds")
	}
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
	latestUser := -1
	inputs := make([]int, 0, 4)
	normal, steers := 0, 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && !messages[i].Runtime {
			if latestUser < 0 {
				latestUser = i
			}
			source, err := inputSource(messages[i])
			if err != nil {
				return Retention{}, fmt.Errorf("human input %d: %w", i, err)
			}
			if source == "steer" {
				if steers < 2 {
					inputs = append(inputs, i)
					steers++
				}
			} else if normal < 2 {
				inputs = append(inputs, i)
				normal++
			}
		}
	}
	if latestUser < 0 {
		return fail("no human input to retain")
	}
	sort.Ints(inputs)
	threshold, cycles := len(messages), 0
	for i := len(messages) - 1; i > latestUser; i-- {
		if messages[i].Role == "assistant" {
			threshold = i
			cycles++
			if cycles == 2 {
				break
			}
		}
	}
	target := min(maxTokens, max(minTokens, suffix[threshold]))
	cut := len(messages)
	for i := latestUser + 1; i < len(messages); i++ {
		if messages[i].Role == "assistant" && safe[i] && suffix[i] <= target {
			cut = i
			break
		}
	}
	// A metadata-only summary cannot substitute for a native attachment that
	// the coding model has never seen. A later assistant response establishes
	// consumption; human/runtime messages alone do not.
	latestAssistant := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			latestAssistant = i
			break
		}
	}
	if latestAssistant >= 0 {
		for _, message := range messages[latestAssistant+1:] {
			if message.Role == "tool" && len(message.Files) > 0 {
				// Overlapping calls can leave the latest assistant inside an
				// earlier cycle. Preserve all calls needed to pair its results.
				start := latestAssistant
				for start >= 0 && (messages[start].Role != "assistant" || !safe[start]) {
					start--
				}
				if start < 0 {
					return fail("unread binary result has no complete assistant cycle to retain")
				}
				cut = min(cut, start)
				break
			}
		}
	}
	// Inputs already inside a protected cycle are retained by the suffix, not
	// copied a second time as independently selected human instructions.
	inputs = inputs[:sort.SearchInts(inputs, cut)]
	// Selected inputs and runtime metadata alone are not a useful summary prefix.
	// An unselected older human input does make progress, even without model work.
	for i, message := range messages[:cut] {
		selected := sort.SearchInts(inputs, i)
		if !message.Runtime && (message.Role == "assistant" || message.Role == "user" && (selected == len(inputs) || inputs[selected] != i)) {
			return Retention{Start: cut, Inputs: inputs}, nil
		}
	}
	return fail("no unretained human input or completed older model cycle can be compacted")
}

func inputSource(message provider.Message) (string, error) {
	switch message.InputSource {
	case "":
		return "normal", nil
	case "normal", "queue", "steer", "task", "btw":
		return message.InputSource, nil
	default:
		return "", fmt.Errorf("invalid human input source %q", message.InputSource)
	}
}

// Attachment is a snapshot made before queuing user input; bytes do not change on disk edits.
type Attachment struct {
	Path      string               `json:"path"`
	Kind      string               `json:"kind"`
	Text      string               `json:"text,omitempty"`
	Image     *provider.BinaryFile `json:"image,omitempty"`
	Truncated bool                 `json:"truncated"`
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
	// Open first: a concurrent replacement with a FIFO must not block before
	// cancellation is installed. Explicit file symlinks remain supported.
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if e != nil {
		return Attachment{}, e
	}
	defer f.Close()
	return snapshotOpened(ctx, path, f, images)
}

// snapshotOpened reads only the caller-owned descriptor, never reopening path.
func snapshotOpened(ctx stdcontext.Context, path string, f *os.File, images bool) (Attachment, error) {
	stop := stdcontext.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()
	st, e := f.Stat()
	if err := ctx.Err(); err != nil {
		return Attachment{}, err
	}
	if e != nil {
		return Attachment{}, e
	}
	a := Attachment{Path: path}
	if st.IsDir() {
		a.Kind = "directory"
		paths := []string{}
		var walk func(string, *os.File) error
		walk = func(dir string, opened *os.File) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			for len(paths) < 500 {
				if err := ctx.Err(); err != nil {
					return err
				}
				entries, err := opened.ReadDir(min(128, 500-len(paths)))
				if ctx.Err() != nil {
					return ctx.Err()
				}
				for _, d := range entries {
					p := filepath.Join(dir, d.Name())
					rel, err := filepath.Rel(path, p)
					if err != nil {
						return err
					}
					paths = append(paths, rel)
					if d.IsDir() && len(paths) < 500 {
						// Resolve relative to the already-open directory so renamed
						// ancestors cannot redirect traversal through a symlink.
						fd, err := syscall.Openat(int(opened.Fd()), d.Name(), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
						if err != nil {
							return err
						}
						child := os.NewFile(uintptr(fd), p)
						stop := stdcontext.AfterFunc(ctx, func() { _ = child.Close() })
						err = walk(p, child)
						stop()
						_ = child.Close()
						if err != nil {
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
		e = walk(path, f)
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
		a.Image = &provider.BinaryFile{Path: path, DataURL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b), MIMEType: mime, Bytes: len(b)}
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

// Input retains the authored text and attachment snapshots of unadmitted input.
// Its attachments are immutable and must not be changed by callers after queuing.
type Input struct {
	Text        string
	Source      string // Admission source, passed unchanged to the model-facing message.
	Attachments []Attachment
}

// Message constructs the model-facing message without rereading attachment paths.
func (i Input) Message() provider.Message {
	if len(i.Attachments) == 0 {
		return provider.Message{Role: "user", Content: i.Text, InputSource: i.Source}
	}
	text := i.Text
	m := provider.Message{Role: "user", UserText: &text, InputSource: i.Source}
	var content strings.Builder
	content.WriteString(text)
	for _, a := range i.Attachments {
		if a.Image != nil {
			m.Files = append(m.Files, *a.Image)
			content.WriteString("\nImage attachment: ")
			content.WriteString(a.Path)
			continue
		}
		content.WriteString("\n\nAttachment (")
		content.WriteString(a.Kind)
		content.WriteString("): ")
		content.WriteString(a.Path)
		content.WriteString("\n")
		content.WriteString(a.Text)
		if a.Truncated {
			content.WriteString("\n[attachment truncated]")
		}
	}
	m.Content = content.String()
	return m
}
