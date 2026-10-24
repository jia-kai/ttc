package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
	"ttc/internal/render"
	"ttc/internal/tui"
)

func markdownPath(args []string, output io.Writer) (string, error) {
	flags := flag.NewFlagSet("ttc md", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.Usage = func() {
		fmt.Fprintln(output, "Usage: ttc md FILE")
		fmt.Fprintln(output, "View Markdown without an agent session. Ctrl-U/D scroll; q quits; Esc goes back or quits.")
		fmt.Fprintln(output, "Redirected output is plain rendered text, wrapped at 80 columns.")
	}
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if flags.NArg() != 1 {
		return "", errors.New("usage: ttc md FILE (exactly one Markdown file is required)")
	}
	return flags.Arg(0), nil
}

func runMarkdown(args []string) error {
	path, err := markdownPath(args, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	source, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read Markdown: %w", err)
	}
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		text, err := render.Terminal(string(source), 80)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(os.Stdout, strings.Trim(ansi.Strip(text), "\n"))
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	document := tui.Document{Title: filepath.Base(path), Text: string(source), Directory: filepath.Dir(path)}
	return document.Run(ctx)
}
