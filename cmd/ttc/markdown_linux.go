package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
	"ttc/internal/assets"
	"ttc/internal/binaryinput"
	"ttc/internal/render"
	"ttc/internal/tui"
)

func viewFile(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".md" || ext == ".markdown" {
		return viewMarkdown(ctx, path)
	}
	// Image detection is signature-based, just like native image input. Reuse
	// that boundary's limits and complete single-frame GIF validation rather
	// than silently displaying an animation's first frame.
	data, err := assets.ReadEncoded(path)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}
	pixels, err := binaryinput.DecodeImage(ctx, data)
	if err != nil {
		return fmt.Errorf("unsupported file format or invalid image %q; expected Markdown (.md, .markdown), PNG, JPEG or single-frame GIF: %w", path, err)
	}
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return fmt.Errorf("image viewing requires an interactive Kitty graphics terminal")
	}
	viewer := tui.Image{Path: path, Pixels: pixels}
	return viewer.Run(ctx)
}

func viewMarkdown(ctx context.Context, path string) error {
	source, err := assets.ReadEncoded(path)
	if err != nil {
		return fmt.Errorf("read Markdown: %w", err)
	}
	if !utf8.Valid(source) || bytes.IndexByte(source, 0) >= 0 {
		return fmt.Errorf("Markdown must be UTF-8 text without NUL bytes: %q", path)
	}
	if len(source) != 0 && !strings.HasPrefix(http.DetectContentType(source), "text/") {
		return fmt.Errorf("unsupported binary content in Markdown file: %q", path)
	}
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		text, err := render.Terminal(string(source), 80)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(os.Stdout, strings.Trim(ansi.Strip(text), "\n"))
		return err
	}
	document := tui.Document{Title: filepath.Base(path), Text: string(source), Directory: filepath.Dir(path)}
	return document.Run(ctx)
}
