package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"unicode/utf8"

	"scicode/internal/scratch"
)

const maxDraftBytes = 8 << 20

// editDraft owns a private, disposable input file. Editor settings are shell
// commands, so arguments such as "vim -f" work; the filename is passed separately.
// Terminal suspension/resumption belongs to the caller. Failure preserves input.
func editDraft(ctx context.Context, text string) (string, error) {
	root, err := scratch.Verify()
	if err != nil {
		return text, err
	}
	dir, err := os.MkdirTemp(root, "editor-")
	if err != nil {
		return text, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "input.md")
	if err = os.WriteFile(path, []byte(text), 0600); err != nil {
		return text, err
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return text, fmt.Errorf("open editor terminal: %w", err)
	}
	defer tty.Close()
	cmd := exec.CommandContext(ctx, "sh", "-c", "exec "+editor+` "$1"`, "ttc-editor", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	if err = cmd.Run(); err != nil {
		return text, fmt.Errorf("editor: %w", err)
	}
	return readEditedDraft(path, text)
}

func readEditedDraft(path, original string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return original, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return original, err
	}
	if !st.Mode().IsRegular() {
		return original, errors.New("edited input must be a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxDraftBytes+1))
	if err != nil {
		return original, err
	}
	if len(b) > maxDraftBytes || !utf8.Valid(b) {
		return original, errors.New("edited input exceeds 8 MiB or is not UTF-8")
	}
	return string(b), nil
}
