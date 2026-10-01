package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"scicode/internal/workspace"
)

const searchTextLimit = 40000

func addSearch(r *Registry, w *workspace.Manager) {
	Register(r, "glob", "Find paths using native ripgrep globs. Positive globs override ignore and hidden-file rules. Returns sorted bounded paths.", map[string]any{"pattern": Property("string"), "path": Property("string"), "hidden": Property("boolean"), "limit": Property("integer")}, []string{"pattern"}, func(a globArgs) error {
		if err := Required("pattern", a.Pattern); err != nil {
			return err
		}
		return rangeInt("limit", a.Limit, 1, 500)
	}, func(ctx context.Context, x Execution, a globArgs) (any, error) {
		root := w.Root
		if a.Path != "" {
			root = w.Path(a.Path)
		}
		args := []string{"--no-config", "--files", "--no-require-git", "--null", "--glob", a.Pattern}
		if a.Hidden {
			args = append(args, "--hidden")
		}
		paths, size := []string{}, 0
		truncated, err := runSearch(ctx, root, args, splitNull, func(token []byte) (bool, error) {
			if !utf8.Valid(token) {
				return false, Fail("unsupported_content", "file path is not UTF-8")
			}
			if len(paths) >= intDefault(a.Limit, 100) || size+len(token) > searchTextLimit {
				return true, nil
			}
			paths = append(paths, string(token))
			size += len(token)
			return false, nil
		})
		if err != nil {
			return nil, err
		}
		sort.Strings(paths)
		return map[string]any{"root": root, "paths": paths, "truncated": truncated}, nil
	})
	Register(r, "grep", "Search with ripgrep regular expressions or literal text. Native glob and ignore rules apply. Returns sorted bounded matching lines.", map[string]any{"pattern": Property("string"), "path": Property("string"), "include": Property("string"), "literal": Property("boolean"), "case_sensitive": Property("boolean"), "limit": Property("integer")}, []string{"pattern"}, func(a grepArgs) error {
		if a.Pattern == "" {
			return errors.New("pattern required")
		}
		return rangeInt("limit", a.Limit, 1, 500)
	}, func(ctx context.Context, x Execution, a grepArgs) (any, error) {
		root := w.Root
		if a.Path != "" {
			root = w.Path(a.Path)
		}
		st, err := os.Stat(root)
		if err != nil {
			return nil, err
		}
		target := "."
		if !st.IsDir() {
			if !st.Mode().IsRegular() {
				return nil, Fail("unsupported_content", "search path is not a regular file or directory")
			}
			target, root = "./"+filepath.Base(root), filepath.Dir(root)
		}
		args := []string{"--no-config", "--json", "--no-require-git", "--color=never"}
		if a.Literal {
			args = append(args, "--fixed-strings")
		}
		if a.CaseSensitive != nil && !*a.CaseSensitive {
			args = append(args, "--ignore-case")
		} else {
			args = append(args, "--case-sensitive")
		}
		if a.Include != "" {
			args = append(args, "--glob", a.Include)
		}
		args = append(args, "--regexp", a.Pattern, "--", target)
		matches, size := []searchMatch{}, 0
		truncated, err := runSearch(ctx, root, args, bufio.ScanLines, func(token []byte) (bool, error) {
			var event struct {
				Type string `json:"type"`
				Data struct {
					Path  rgText `json:"path"`
					Lines rgText `json:"lines"`
					Line  int    `json:"line_number"`
				} `json:"data"`
			}
			if err := json.Unmarshal(token, &event); err != nil {
				return false, fmt.Errorf("decode rg output: %w", err)
			}
			if event.Type != "match" {
				return false, nil
			}
			path, err := event.Data.Path.string()
			if err != nil {
				return false, err
			}
			text, err := event.Data.Lines.string()
			if err != nil {
				return false, err
			}
			if !utf8.ValidString(path) {
				return false, Fail("unsupported_content", "file path is not UTF-8")
			}
			if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
				return false, nil // Keep the text tool's UTF-8 contract for non-text matches.
			}
			text = strings.TrimSuffix(strings.TrimSuffix(text, "\n"), "\r")
			if len(matches) >= intDefault(a.Limit, 100) || size+len(text) > searchTextLimit {
				return true, nil
			}
			rel, err := filepath.Rel(w.Root, filepath.Join(root, path))
			if err != nil {
				return false, err
			}
			matches = append(matches, searchMatch{Path: rel, Line: event.Data.Line, Text: text})
			size += len(text)
			return false, nil
		})
		if err != nil {
			return nil, err
		}
		sort.Slice(matches, func(i, j int) bool {
			if matches[i].Path != matches[j].Path {
				return matches[i].Path < matches[j].Path
			}
			return matches[i].Line < matches[j].Line
		})
		return map[string]any{"matches": matches, "truncated": truncated}, nil
	})
}

type searchMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// rgText is ripgrep's lossless JSON text-or-base64 encoding, also used for file paths.
type rgText struct {
	Text  *string `json:"text"`
	Bytes string  `json:"bytes"`
}

func (v rgText) string() (string, error) {
	if v.Text != nil {
		return *v.Text, nil
	}
	b, err := base64.StdEncoding.DecodeString(v.Bytes)
	return string(b), err
}

// runSearch streams one rg process and cancels/reaps it as soon as the result budget is full.
// Sorting only the bounded results preserves rg's parallel search rather than forcing a serial traversal.
func runSearch(ctx context.Context, dir string, args []string, split bufio.SplitFunc, consume func([]byte) (bool, error)) (truncated bool, err error) {
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(child, "rg", args...)
	cmd.Dir = dir
	stderr := &searchErrors{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	if err := cmd.Start(); err != nil {
		stdout.Close()
		return false, fmt.Errorf("start rg: %w", err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	scanner.Split(split)
	for scanner.Scan() {
		truncated, err = consume(scanner.Bytes())
		if truncated || err != nil {
			cancel()
			break
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		cancel()
		if errors.Is(scanErr, bufio.ErrTooLong) {
			truncated = true
		} else {
			err = scanErr
		}
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil || truncated {
		return truncated, err
	}
	if waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) || exit.ExitCode() != 1 {
			return false, fmt.Errorf("rg search failed: %w: %s", waitErr, strings.TrimSpace(string(stderr.data)))
		}
	}
	return false, nil
}

type searchErrors struct{ data []byte }

func (w *searchErrors) Write(p []byte) (int, error) {
	w.data = append(w.data, p[:min(len(p), (8<<10)-len(w.data))]...)
	return len(p), nil
}

func splitNull(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
