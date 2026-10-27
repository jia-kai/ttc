package tui

import (
	"context"
	"errors"
	"fmt"
	"image"
	"path/filepath"
	"strings"

	"github.com/gdamore/tcell/v3"
	"github.com/mattn/go-runewidth"
	"ttc/internal/graphics"
	"ttc/internal/render"
)

// Document displays Markdown without a session, sidebar or composer.
// Run owns terminal initialization/finalization and graphics worker lifetimes.
// Fields must not be changed while Run is active.
type Document struct {
	Directory string          // Base directory for relative local Markdown image paths; empty uses cwd.
	Title     string          // Filename or short label displayed in the bottom status row.
	Text      string          // Complete Markdown source; empty documents are supported.
	Screen    tcell.Screen    // Optional uninitialized screen, primarily for tests.
	Graphics  *graphics.Kitty // Optional graphics sink used with an injected Screen.
}

// Run shows the document until q, Esc or Ctrl-C is pressed. Esc first returns
// from an image preview or cancels its pending load. User exits return nil;
// context cancellation returns ctx.Err(). Terminal and graphics errors are
// returned after canceling and joining workers and restoring the terminal.
func (d *Document) Run(ctx context.Context) error {
	return d.run(ctx, nil)
}

// Image displays already validated, decoded image pixels in the shared pan/zoom
// preview without an agent session. Pixels must not be mutated during Run.
type Image struct {
	Path     string          // Source filename shown in the preview status.
	Pixels   image.Image     // Required decoded image, with positive dimensions.
	Screen   tcell.Screen    // Optional uninitialized screen, primarily for tests.
	Graphics *graphics.Kitty // Optional graphics sink used with an injected Screen.
}

// Run owns the terminal and graphics workers until q, Esc or Ctrl-C is pressed.
// It fails when pixels are missing or the terminal does not support graphics.
func (i *Image) Run(ctx context.Context) error {
	if i.Pixels == nil || i.Pixels.Bounds().Empty() {
		return fmt.Errorf("image pixels with positive dimensions are required")
	}
	d := Document{Title: filepath.Base(i.Path), Screen: i.Screen, Graphics: i.Graphics}
	return d.run(ctx, i)
}

func (d *Document) run(ctx context.Context, initial *Image) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	terminal, err := openTerminalView(ctx, d.Screen, d.Graphics)
	if err != nil {
		return err
	}
	defer terminal.close()
	if initial != nil && terminal.graphics == nil {
		return fmt.Errorf("image viewing requires Kitty terminal graphics: %s", terminal.warning)
	}
	window := &Window{Text: d.Text, Markdown: true}
	var results <-chan renderReply
	if initial != nil {
		// A decoded standalone image only needs placement and cell sizing. Do
		// not start the Markdown worker or its MathJax installer.
		window.renderer = &imageRenderer{graphics: terminal.graphics, cellWidth: 8, cellHeight: 16}
		defer func() { err = errors.Join(err, terminal.graphics.Close()) }()
	} else if terminal.graphics != nil {
		renderer, e := newImageRenderer(ctx, terminal.graphics, d.Directory)
		if e != nil {
			return errors.Join(e, terminal.graphics.Close())
		}
		window.renderer = renderer
		results = renderer.results
		defer func() { err = errors.Join(err, renderer.close()) }()
	}
	warning := terminal.warning
	var preview *imagePreview
	if initial != nil {
		preview = newImagePreview(imageSource{id: initial.Path, path: initial.Path, file: initial.Path}, initial.Pixels, false, window.renderer.cellWidth, window.renderer.cellHeight)
		preview.Window.Hint = "h/j/k/l pan · +/- zoom · 0 fit · i details · q/Esc quit"
	}
	var loading *imageLoad
	defer func() {
		if loading != nil {
			loading.cancel()
		}
	}()
	activation := terminal.activation
	keys := terminal.screen.EventQ()
	for {
		if err := drawDocument(terminal, window, d.Title, &warning, preview); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-activation:
			if !ok {
				activation = nil
				warning = "Tmux image recovery observer stopped"
			} else {
				refreshTerminalGraphics(terminal.screen, terminal.graphics, false)
			}
		case reply := <-results:
			if reply.source {
				if loading != nil && loading.key == reply.key {
					loading.cancel()
					if reply.err != nil {
						warning = "Image preview failed: " + reply.err.Error()
					} else {
						preview = newImagePreview(loading.source, reply.pixels, false, window.renderer.cellWidth, window.renderer.cellHeight)
						warning = ""
					}
					loading = nil
				}
				continue
			}
			if window.renderer.accept(reply) {
				failure := reply.err
				if reply.backend == "" {
					failure = window.renderer.ready[reply.key].err
				}
				if failure != nil {
					warning = "Math/image rendering failed: " + failure.Error()
				}
			}
		case event, ok := <-keys:
			if !ok {
				return nil
			}
			_, height := terminal.screen.Size()
			switch event := event.(type) {
			case *tcell.EventError:
				return fmt.Errorf("terminal input: %w", event)
			case *tcell.EventKey:
				if isCtrl(event, "c") {
					return nil
				}
				if preview != nil {
					if close, _ := preview.key(event); close {
						if initial != nil {
							return nil
						}
						preview = nil
					}
					continue
				}
				if event.Key() == tcell.KeyRune && event.Str() == "q" {
					return nil
				}
				if event.Key() == tcell.KeyEscape {
					if loading != nil {
						loading.cancel()
						loading = nil
						warning = ""
						continue
					}
					return nil
				}
				window.Key(event, documentHeight(height))
			case *tcell.EventResize:
				terminal.screen.Sync()
			case *tcell.EventFocus:
				if event.Focused && terminal.graphics != nil && !terminal.graphics.Tmux {
					refreshTerminalGraphics(terminal.screen, terminal.graphics, false)
				}
			case *tcell.EventMouse:
				if preview != nil {
					if close, _ := preview.mouse(event); close {
						if initial != nil {
							return nil
						}
						preview = nil
					}
					continue
				}
				if window.renderer != nil && event.Buttons()&tcell.Button1 != 0 {
					x, y := event.Position()
					if y >= 0 && y < documentHeight(height) {
						if asset := window.imageAt(x, y); asset != nil {
							if loading != nil {
								loading.cancel()
							}
							var err error
							loading, err = window.renderer.loadSource(imageSource{id: asset.key, path: asset.path, file: asset.path})
							if err != nil {
								warning = "Image preview failed: " + err.Error()
							} else {
								warning = "Loading full image…"
							}
						}
					}
				}
				if event.Buttons()&tcell.WheelUp != 0 {
					window.ScrollBy(-3, documentHeight(height))
				}
				if event.Buttons()&tcell.WheelDown != 0 {
					window.ScrollBy(3, documentHeight(height))
				}
			}
		}
	}
}

// Keep one content row even in a one-row terminal; omit its status instead.
func documentHeight(height int) int {
	if height > 1 {
		return height - 1
	}
	return max(0, height)
}

func drawDocument(terminal *terminalView, window *Window, title string, warning *string, preview *imagePreview) error {
	screen := terminal.screen
	width, height := screen.Size()
	bodyHeight := documentHeight(height)
	renderer := window.renderer
	if renderer != nil {
		var size tcell.WindowSize
		if terminal.tty != nil {
			if measured, err := terminal.tty.WindowSize(); err == nil {
				size = measured
			}
		}
		if _, message := renderer.updateCellDimensions(size); message != "" && *warning == "" {
			*warning = message
		}
		terminal.graphics.Begin()
	}
	if preview != nil && renderer != nil {
		preview.cellWidth, preview.cellHeight = renderer.cellWidth, renderer.cellHeight
		if err := renderer.ensure(nil); err != nil {
			return err
		}
		screen.Clear()
		screen.HideCursor()
		if err := preview.draw(screen, terminal.graphics); err != nil {
			drawWindow(screen, &Window{Title: "Image preview", Text: "Image preview failed: " + err.Error()})
		}
		screen.Show()
		return terminal.graphics.End()
	}
	rows := window.Lines(width, bodyHeight)
	if renderer != nil {
		visible := make([]line, len(rows))
		for i := range rows {
			row := window.assets[window.Scroll+i]
			visible[i] = line{text: row.text, assets: row.assets}
		}
		if err := renderer.ensure(visible); err != nil {
			return err
		}
	}
	screen.Clear()
	screen.HideCursor()
	style := tcell.StyleDefault.Foreground(tcell.GetColor(render.TextColor))
	for y, row := range rows {
		putStyled(screen, 0, y, width, row, style)
	}
	if height > bodyHeight {
		progress := scrollIndicator(len(window.cachedLines), bodyHeight, window.Scroll, width)
		available := max(0, width-runewidth.StringWidth(progress)-1)
		status := render.Clean(title) + " · q quit · Ctrl-U/D scroll"
		if *warning != "" {
			status = strings.Join(strings.Fields(render.Clean(*warning)), " ")
		}
		statusStyle := style.Foreground(tcell.GetColor(render.MutedColor))
		put(screen, 0, height-1, available, status, statusStyle)
		put(screen, max(0, width-runewidth.StringWidth(progress)), height-1, width, progress, statusStyle)
	}
	screen.Show()
	if renderer != nil {
		return terminal.graphics.End()
	}
	return nil
}
