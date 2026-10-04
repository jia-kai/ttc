// TTC is a Linux terminal coding agent with subscription-only OpenAI transport.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"ttc/internal/assets"
	"ttc/internal/history"
	"ttc/internal/provider"
	"ttc/internal/provider/openai"
	"ttc/internal/rail"
	"ttc/internal/scratch"
	"ttc/internal/session"
	"ttc/internal/skills"
	"ttc/internal/tui"
	"ttc/internal/workspace"

	"golang.org/x/term"
)

type loginUI struct{}

func (loginUI) Present(ctx context.Context, step provider.LoginStep) (provider.LoginAnswer, error) {
	if step.Kind == "device_code" {
		fmt.Fprintf(os.Stdout, "Authorize from another device: %s\nCode: %s (expires in %ds)\n", step.URL, step.Code, step.ExpiresSeconds)
	} else {
		fmt.Fprintln(os.Stdout, step.Message)
	}
	return provider.LoginAnswer{}, nil
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, "ttc:", e)
		os.Exit(1)
	}
}
func run() error {
	if rail.IsClientServer(os.Args[1:]) {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return rail.ServeClients(ctx)
	}
	if rail.IsSupervisor(os.Args[1:]) {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return rail.Serve(ctx)
	}
	if len(os.Args) > 1 && os.Args[1] == "rail" {
		return runRail(os.Args[2:])
	}
	rootDefault, dataDefaultErr := history.DataRoot()
	searchPath, e := webSearchConfigPath()
	if e != nil {
		return e
	}
	searchConfigFile := flag.String("web-search-config", searchPath, "operator web-search JSON configuration")
	data := flag.String("data-dir", rootDefault, "private data root")
	cwd := flag.String("workdir", ".", "workspace directory")
	plain := flag.Bool("plain", false, "plain terminal output")
	importAuth := flag.String("import-codex-auth", "", "explicitly copy ChatGPT subscription credentials from a file")
	login := flag.Bool("login", false, "run device-code login then exit")
	installMath := flag.Bool("install-math", false, "install pinned MathJax in the user cache then exit")
	endpoint := flag.String("openai-base-url", "", "explicit subscription endpoint override for local mock-server tests")
	model := flag.String("model", "", "optional model ID override (otherwise use last choice)")
	variant := flag.String("variant", "", "optional reasoning preset override")
	script := flag.String("offline-script", "", "JSON response script for deterministic offline integration")
	load := flag.String("session", "", "load stored session ID")
	autoName := flag.Bool("auto-name", true, "one small naming request at the first tool boundary or completed response")
	flag.Parse()
	if e = applySSHAuthSockPolicy(); e != nil {
		return e
	}
	if *installMath {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		path, err := assets.InstallMath(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "MathJax ready:", path)
		return nil
	}
	if e = dataDefaultError(flag.CommandLine, dataDefaultErr); e != nil {
		return e
	}
	searchConfig, e := loadWebSearchConfig(*searchConfigFile)
	if e != nil {
		return e
	}
	if endpoint := os.Getenv("TTC_EXA_URL"); endpoint != "" {
		searchConfig.Endpoint = endpoint
	}
	if e = searchConfig.Validate(); e != nil {
		return fmt.Errorf("web search config: %w", e)
	}
	if _, e := exec.LookPath("rg"); e != nil {
		return fmt.Errorf("ripgrep (rg) is required for local search; install ripgrep and ensure rg is on PATH: %w", e)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if _, e = scratch.Verify(); e != nil {
		return e
	}
	store, e := history.Open(*data)
	if e != nil {
		return e
	}
	defer store.Close()
	w, e := workspace.Open(*cwd, store)
	if e != nil {
		return e
	}
	var p provider.Provider
	adapter := openai.New(filepath.Join(store.Root, "openai-auth.json"))
	if *endpoint != "" {
		adapter.BaseURL = *endpoint
	}
	if *importAuth != "" {
		if e = adapter.ImportCodex(*importAuth); e != nil {
			return e
		}
		fmt.Fprintln(os.Stderr, "Imported subscription credentials into private TTC storage")
	}
	if *login {
		return adapter.Login(ctx, loginUI{})
	}
	if *script != "" {
		b, e := os.ReadFile(*script)
		if e != nil {
			return e
		}
		var responses []provider.ScriptResponse
		if e = json.Unmarshal(b, &responses); e != nil {
			return e
		}
		p = &provider.Script{Responses: responses}
		*model = "scripted"
		*autoName = false
	} else {
		p = adapter
	}
	modelsCtx, modelsCancel := context.WithTimeout(ctx, 30*time.Second)
	models, e := p.Models(modelsCtx)
	modelsCancel()
	if e != nil {
		return e
	}
	providerID := "openai"
	if *script != "" {
		providerID = "script"
	}
	var remembered *provider.Selection
	if *model == "" {
		remembered, e = store.LastSelection(providerID)
		if e != nil {
			return fmt.Errorf("read saved model choice: %w", e)
		}
	}
	selection, notice, e := startupSelection(providerID, models, remembered, *model, *variant)
	if e != nil {
		return e
	}
	if notice != "" {
		fmt.Fprintln(os.Stderr, "ttc:", notice)
	}
	var saved history.Session
	if *load != "" {
		saved, e = store.Session(*load)
		if e == nil {
			var path string
			e = store.DB.QueryRow("SELECT path FROM workspaces WHERE id=?", saved.WorkspaceID).Scan(&path)
			if e == nil && path != w.Root {
				return fmt.Errorf("session belongs to another workspace")
			}
		}
		if e == nil {
			saved, e = store.Load(*load)
		}
	}
	if e != nil {
		return e
	}
	if saved.ReadOnly {
		selection = saved.Model
	} else if e = store.SaveSelection(selection); e != nil {
		return e
	}
	if !saved.ReadOnly && *load != "" && (saved.Model.Provider != selection.Provider || saved.Model.Model.ID != selection.Model.ID || saved.Model.Variant != selection.Variant) {
		text := fmt.Sprintf("Model switched · %s · %s", selection.Model.ID, selection.Variant)
		if _, e := store.SwitchModel(saved.ID, "", saved.Model, selection, text); e != nil {
			return e
		}
	}
	home, e := os.UserHomeDir()
	if e != nil {
		return e
	}
	catalog, e := skills.Discover(ctx, w.Root, filepath.Join(home, ".agents/skills"))
	if e != nil {
		return e
	}
	events := make(chan session.Event, 256)
	runtime := session.New(ctx, store, w, p, selection, saved.ID, catalog, searchConfig, func(event session.Event) {
		select {
		case events <- event:
		case <-ctx.Done():
		}
	})
	runtime.AutoName = *autoName
	defer func() {
		// Stop UI delivery before joining callbacks after the frontend has exited.
		cancel()
		runtime.Close()
	}()
	ui := &tui.Frontend{Runtime: runtime, Events: events, Input: os.Stdin, Output: os.Stdout, Plain: *plain || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())), Login: loginUI{}, Models: models}
	return ui.Run(ctx)
}
