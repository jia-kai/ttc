package main

import (
	"context"
	"fmt"
	"github.com/spf13/cobra"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
	"ttc/internal/render"
	"ttc/internal/tui"
)

func newMarkdownCommand() *cobra.Command {
	return &cobra.Command{Use: "md FILE", Short: "View Markdown without an agent session", Long: "View Markdown without an agent session. Ctrl-U/D scroll; q quits; Esc goes back or quits. Redirected output is plain rendered text, wrapped at 80 columns.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return viewMarkdown(cmd.Context(), args[0])
	}}
}

func viewMarkdown(ctx context.Context, path string) error {
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
	document := tui.Document{Title: filepath.Base(path), Text: string(source), Directory: filepath.Dir(path)}
	return document.Run(ctx)
}
