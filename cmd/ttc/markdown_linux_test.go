package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarkdownPath(t *testing.T) {
	for _, args := range [][]string{nil, {"a", "b"}, {"--unknown"}} {
		if _, err := markdownPath(args, new(bytes.Buffer)); err == nil {
			t.Fatalf("accepted arguments %q", args)
		}
	}
	var help bytes.Buffer
	if _, err := markdownPath([]string{"--help"}, &help); !errors.Is(err, flag.ErrHelp) || !strings.Contains(help.String(), "Ctrl-U/D") {
		t.Fatalf("help: %v, %q", err, help.String())
	}
	if path, err := markdownPath([]string{"--", "-notes.md"}, &help); err != nil || path != "-notes.md" {
		t.Fatalf("literal path: %q, %v", path, err)
	}
}

func TestMarkdownCommandBypassesAgentStartup(t *testing.T) {
	// No configured provider, valid configuration root or history is needed.
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "/invalid/config")
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	missing := filepath.Join(t.TempDir(), "missing.md")
	os.Args = []string{"ttc", "md", missing}
	if err := runWithModules(nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected file error before agent setup: %v", err)
	}
	os.Args = []string{"ttc", "md", "--help"}
	if err := runWithModules(nil); err != nil {
		t.Fatal(err)
	}
}

func TestMarkdownRedirectedOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(path, []byte("# Notes\n\n- **first**\n\n  $$x+y$$\n\n  tail\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	oldOutput, oldArgs := os.Stdout, os.Args
	t.Cleanup(func() { os.Stdout, os.Args = oldOutput, oldArgs })
	os.Stdout, os.Args = writer, []string{"ttc", "md", path}
	if err := runWithModules(nil); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	for _, want := range []string{"Notes", "first", "x+y", "tail"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	if strings.ContainsAny(text, "\x1b$") {
		t.Fatalf("output contains ANSI or math delimiters: %q", text)
	}
}
