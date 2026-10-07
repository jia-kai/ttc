package tui

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"ttc/internal/catalog"
	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/session"
	"ttc/internal/skills"
	"ttc/internal/tool"
	"ttc/internal/workspace"
)

type catalogLoginProvider struct {
	llm.Script
	started, release chan struct{}
	fresh            []llm.ModelSpec
	err              error
}

func (p *catalogLoginProvider) authorize(ctx context.Context) ([]llm.ModelSpec, error) {
	close(p.started)
	select {
	case <-p.release:
		return p.fresh, p.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestLoginDiscardsStartupRefreshAndDiscoversCurrentAccount(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "success"
		if failed {
			name = "discovery failure"
		}
		t.Run(name, func(t *testing.T) {
			store, err := history.Open(filepath.Join(t.TempDir(), "data"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			w, err := workspace.Open(t.TempDir(), store)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			initial := llm.Selection{Provider: "script", Model: menuModels()[0], Variant: "low"}
			fresh := menuModels()[:1]
			fresh[0].Name = "Current account model"
			p := &catalogLoginProvider{started: make(chan struct{}), release: make(chan struct{}), fresh: fresh}
			if failed {
				p.err = errors.New("discovery unavailable")
			}
			events := make(chan session.Event, 64)
			r, err := session.New(ctx, store, w, p, initial, "", &skills.Catalog{}, tool.WebSearchConfig{}, func(e session.Event) {
				select {
				case events <- e:
				case <-ctx.Done():
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			r.AutoName = false
			defer r.Close()
			input, writer := io.Pipe()
			defer input.Close()
			defer writer.Close()
			output := make(chan string, 128)
			updates := make(chan catalog.Update, 1)
			f := Frontend{Runtime: r, Events: events, Plain: true, Input: input, Output: eventWriter{output}, Models: menuModels(), ModelUpdates: updates, Login: func(ctx context.Context) ([]llm.ModelSpec, error) {
				models, err := p.authorize(ctx)
				// The injected owner, not the UI, invalidates old deliveries.
				for range updates {
				}
				return models, err
			}}
			done := make(chan error, 1)
			go func() { done <- f.Run(ctx) }()
			joined := false
			defer func() {
				cancel()
				if !joined {
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Error("frontend did not stop")
					}
				}
			}()
			wait := func(needle string) {
				t.Helper()
				deadline := time.After(3 * time.Second)
				for {
					select {
					case text := <-output:
						if strings.Contains(text, "Superseded account model") {
							t.Fatal("late refresh from old account was published", text)
						}
						if strings.Contains(text, needle) {
							return
						}
					case <-deadline:
						t.Fatalf("missing output %q", needle)
					}
				}
			}
			command := func(text string) {
				t.Helper()
				if _, err := io.WriteString(writer, text+"\n"); err != nil {
					t.Fatal(err)
				}
			}
			wait("TTC · Linux terminal agent")
			command("/login")
			select {
			case <-p.started:
			case <-time.After(3 * time.Second):
				t.Fatal("login not started")
			}
			stale := menuModels()[:1]
			stale[0].Name = "Superseded account model"
			updates <- catalog.Update{Models: stale}
			close(updates)
			close(p.release)
			if failed {
				wait("authorization or model discovery failed")
				command("/model")
				wait("model catalog unavailable")
			} else {
				wait("Login complete")
				command("/model")
				wait("Current account model")
				if !reflect.DeepEqual(r.CurrentSelection(), initial) {
					t.Fatal("login silently changed active model")
				}
				command("/model family-a low")
				wait("Model selected for next tool boundary")
				if r.ModelChoice().Model.Name != "Current account model" {
					t.Fatal("selection used stale metadata")
				}
			}
			command("/quit")
			select {
			case err := <-done:
				joined = true
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("frontend did not exit")
			}
		})
	}
}
