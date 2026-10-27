package main

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileArguments(t *testing.T) {
	for _, command := range newRootCommand(nil).Commands() {
		if command.Name() == "md" {
			t.Fatal("obsolete md subcommand remains registered")
		}
	}
	for _, args := range [][]string{{"md", "notes.md"}, {"a", "b"}, {"--unknown"}, {"--plain", "notes.md"}} {
		cmd := newRootCommand(nil)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted arguments %q", args)
		}
	}
	var help bytes.Buffer
	cmd := newRootCommand(nil)
	cmd.SetArgs([]string{"--help"})
	cmd.SetOut(&help)
	if err := cmd.Execute(); err != nil || !strings.Contains(help.String(), "Ctrl-U/D") {
		t.Fatalf("help: %v, %q", err, help.String())
	}
	if strings.Contains(help.String(), "md FILE") {
		t.Fatal("obsolete md command in help")
	}
	cmd = newRootCommand(nil)
	cmd.SetArgs([]string{"--", "-missing.md"})
	if err := cmd.Execute(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("literal path: %v", err)
	}
}

func TestFileCommandBypassesAgentStartup(t *testing.T) {
	// No configured provider, valid configuration root or history is needed.
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "/invalid/config")
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	missing := filepath.Join(t.TempDir(), "missing.md")
	os.Args = []string{"ttc", missing}
	if err := runWithModules(nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected file error before agent setup: %v", err)
	}
	os.Args = []string{"ttc", "--help"}
	if err := runWithModules(nil); err != nil {
		t.Fatal(err)
	}
}

func TestMarkdownRedirectedOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.MARKDOWN")
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
	os.Stdout, os.Args = writer, []string{"ttc", path}
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

func TestFileFormats(t *testing.T) {
	pixels := image.NewRGBA(image.Rect(0, 0, 2, 2))
	palette := color.Palette{color.Black, color.White}
	frame := image.NewPaletted(pixels.Bounds(), palette)
	var pngData, jpegData, gifData, animated bytes.Buffer
	for _, err := range []error{
		png.Encode(&pngData, pixels), jpeg.Encode(&jpegData, pixels, nil),
		gif.Encode(&gifData, frame, nil),
		gif.EncodeAll(&animated, &gif.GIF{Image: []*image.Paletted{frame, frame}, Delay: []int{0, 0}}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Capture stdout so image tests always exercise the redirected-output path.
	output, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	old := os.Stdout
	os.Stdout = output
	t.Cleanup(func() { os.Stdout = old })
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"valid.PNG", pngData.Bytes(), "interactive Kitty"},
		{"valid.jpeg", jpegData.Bytes(), "interactive Kitty"},
		{"valid.gif", gifData.Bytes(), "interactive Kitty"},
		{"signature.bin", pngData.Bytes(), "interactive Kitty"},
		{"animated.gif", animated.Bytes(), "animated"},
		{"truncated.gif", gifData.Bytes()[:gifData.Len()-1], "unexpected EOF"},
		{"fake.png", []byte("not an image"), "unsupported file format"},
		{"notes.txt", []byte("plain text"), "unsupported file format"},
		{"document.pdf", []byte("%PDF-1.7\n"), "unsupported"},
		{"binary.md", pngData.Bytes(), "UTF-8"},
		{"pdf.md", []byte("%PDF-1.7\n"), "unsupported binary"},
		{"nul.md", []byte("a\x00b"), "UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.name)
			if err := os.WriteFile(path, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := newRootCommand(nil)
			cmd.SetArgs([]string{path})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v; want %q", err, tc.want)
			}
		})
	}
}
