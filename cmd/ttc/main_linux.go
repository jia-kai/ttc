// TTC is a Linux terminal coding agent with subscription-only OpenAI transport.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"ttc/internal/auth"
	"ttc/internal/binaryinput"
	"ttc/internal/catalog"
	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/providers"
	"ttc/internal/providers/builtin"
	"ttc/internal/rail"
	"ttc/internal/scratch"
	"ttc/internal/session"
	"ttc/internal/skills"
	"ttc/internal/tui"
	"ttc/internal/workspace"

	"golang.org/x/term"
)

type loginUI struct{}

func (loginUI) Present(ctx context.Context, step auth.Step) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if step.Kind == "device_code" {
		fmt.Fprintf(os.Stdout, "Authorize from another device: %s\nCode: %s (expires in %ds)\n", step.URL, step.Code, step.ExpiresSeconds)
	} else {
		fmt.Fprintln(os.Stdout, step.Message)
	}
	return nil
}
func main() {
	if e := run(); e != nil {
		var exit *workloadExit
		if errors.As(e, &exit) {
			os.Exit(exit.code)
		}
		fmt.Fprintln(os.Stderr, "ttc:", e)
		os.Exit(1)
	}
}
func run() error {
	return runWithModules(builtin.Modules())
}

// runWithModules keeps command orchestration independent of built-in providers.
// Modules register configuration, but the command owns catalog and UI lifetimes.
func runWithModules(modules []providers.Module) error {
	root := newRootCommand(modules)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return root.ExecuteContext(ctx)
}

type launchOptions struct {
	data, cwd, script, load, providerID string
	setupExa, plain, login              bool
}

// newRootCommand creates independent launch and provider configuration per call.
func newRootCommand(modules []providers.Module) *cobra.Command {
	root := &cobra.Command{Use: "ttc [FILE]", Short: "TTC - True Terminal Coding", Long: "TTC - True Terminal Coding\n\nStart TTC, or view a Markdown (.md, .markdown) or PNG/JPEG/single-frame GIF file without an agent session. Ctrl-U/D scroll Markdown; q quits; Esc goes back or quits. Redirected Markdown is plain text wrapped at 80 columns; images require a Kitty graphics terminal.", Args: cobra.MaximumNArgs(1), SilenceUsage: true, SilenceErrors: true}
	root.CompletionOptions.DisableDefaultCmd = true
	flags := root.Flags()
	rootDefault, dataDefaultErr := history.DataRoot()
	var opts launchOptions
	flags.BoolVar(&opts.setupExa, "setup-exa", false, "read and verify an Exa search API key from stdin, save config, then exit")
	flags.StringVar(&opts.data, "data-dir", rootDefault, "private data root")
	flags.StringVar(&opts.cwd, "workdir", ".", "workspace directory")
	flags.BoolVar(&opts.plain, "plain", false, "plain terminal output")
	flags.BoolVar(&opts.login, "login", false, "run device-code login then exit")
	flags.StringVar(&opts.script, "offline-script", "", "JSON response script for deterministic offline integration")
	flags.StringVar(&opts.load, "session", "", "load stored session ID")
	flags.StringVar(&opts.providerID, "provider", "", "compiled provider ID")
	root.InitDefaultHelpFlag()
	appFlags := make(map[string]bool)
	flags.VisitAll(func(f *pflag.Flag) { appFlags[f.Name] = true })
	configured, configErr := providers.Configure(modules, flags)
	if configErr == nil {
		opts.providerID = configured[0].ID
		flags.Lookup("provider").DefValue = configured[0].ID
	}
	root.AddCommand(newRailCommand(), newWatchCommand())
	for _, hook := range []struct {
		name  string
		serve func(context.Context) error
	}{{"__rail_clients", rail.ServeClients}, {"__rail_serve", rail.Serve}} {
		root.AddCommand(&cobra.Command{Use: hook.name, Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			return hook.serve(cmd.Context())
		}})
	}
	root.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			var conflict string
			flags.Visit(func(f *pflag.Flag) { conflict = f.Name })
			if conflict != "" {
				return fmt.Errorf("file viewing cannot be combined with --%s", conflict)
			}
			return viewFile(cmd.Context(), args[0])
		}
		if configErr != nil {
			return configErr
		}
		searchConfigFile, err := webSearchConfigPath()
		if err != nil {
			return err
		}
		if opts.setupExa {
			var conflict string
			flags.Visit(func(f *pflag.Flag) {
				if !appFlags[f.Name] {
					conflict = f.Name
				}
			})
			if opts.login || opts.script != "" || conflict != "" {
				return errors.New("--setup-exa cannot be combined with --login, --offline-script or provider-specific options")
			}
			return setupExa(cmd.Context(), searchConfigFile, os.Stdin, os.Stderr, http.DefaultClient)
		}
		factory, e := providers.Select(configured, opts.providerID)
		if e != nil {
			return e
		}
		if opts.script != "" {
			var conflict string
			flags.Visit(func(f *pflag.Flag) {
				if f.Name == "provider" || !appFlags[f.Name] {
					conflict = f.Name
				}
			})
			if opts.login || conflict != "" {
				return errors.New("--offline-script cannot be combined with --login, --provider or provider-specific options")
			}
		}
		if err := dataDefaultError(flags, dataDefaultErr); err != nil {
			return err
		}
		return runAgent(cmd.Context(), opts, factory, searchConfigFile)
	}
	return root
}

func runAgent(parent context.Context, opts launchOptions, factory providers.Factory, searchConfigFile string) error {
	if err := applySSHAuthSockPolicy(); err != nil {
		return err
	}
	searchConfig, e := loadWebSearchConfig(searchConfigFile)
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
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if _, e = scratch.Verify(); e != nil {
		return e
	}
	progress := newStartupProgress(os.Stderr)
	store, e := history.OpenWithProgress(opts.data, progress.migration)
	progress.finish("History migration", e)
	if e != nil {
		return e
	}
	defer store.Close()
	w, e := workspace.Open(opts.cwd, store)
	if e != nil {
		return e
	}
	var p session.Inference
	var components providers.Components
	providerID := opts.providerID
	if opts.script != "" {
		b, e := os.ReadFile(opts.script)
		if e != nil {
			return e
		}
		var responses []llm.ScriptResponse
		if e = json.Unmarshal(b, &responses); e != nil {
			return e
		}
		p = &llm.Script{Responses: responses}
		providerID = "script"
	} else {
		components, e = factory(ctx, providers.Environment{DataDir: store.Root, ResolveBinary: binaryinput.Resolve})
		if e != nil {
			return fmt.Errorf("open provider %q: %w", providerID, e)
		}
		if components.Inference == nil {
			return fmt.Errorf("provider %q has no inference component", providerID)
		}
		p = components.Inference
		if components.Notice != "" {
			fmt.Fprintln(os.Stderr, components.Notice)
		}
		if opts.login {
			if components.Authorize == nil {
				return fmt.Errorf("provider %q does not support authorization", providerID)
			}
			if err := store.SaveEasySelection(providerID, nil); err != nil {
				return fmt.Errorf("clear easy model before login: %w", err)
			}
			return components.Authorize(ctx, loginUI{})
		}
	}
	var models []llm.ModelSpec
	var modelUpdates <-chan catalog.Update
	var loginCurrent func(context.Context) ([]llm.ModelSpec, error)
	if opts.script != "" {
		models = []llm.ModelSpec{llm.ScriptModel()}
	} else {
		if components.Catalog == nil || components.Catalog.Bind == nil {
			return fmt.Errorf("provider %q has no model catalog", providerID)
		}
		manager, err := catalog.Open(ctx, catalog.Config{Bind: func(ctx context.Context) (catalog.Binding, error) {
			binding, err := components.Catalog.Bind(ctx)
			if err == nil && binding.Scope.Provider != providerID {
				return catalog.Binding{}, fmt.Errorf("provider %q returned catalog scope %q", providerID, binding.Scope.Provider)
			}
			return binding, err
		}, CachePath: components.Catalog.CachePath, Timeout: 30 * time.Second, Policy: catalog.DefaultPolicy()})
		if err != nil {
			return err
		}
		defer manager.Close()
		models, modelUpdates = manager.Initial, manager.Updates
		if manager.Notice != "" {
			fmt.Fprintln(os.Stderr, "ttc:", manager.Notice)
		}
		if components.Authorize != nil {
			loginCurrent = func(ctx context.Context) ([]llm.ModelSpec, error) {
				manager.Invalidate()
				if err := components.Authorize(ctx, loginUI{}); err != nil {
					return nil, err
				}
				return manager.Refresh(ctx)
			}
		}
	}
	var remembered *llm.Selection
	remembered, e = store.LastSelection(providerID)
	if e != nil {
		return fmt.Errorf("read saved model choice: %w", e)
	}
	selection, notice, e := startupSelection(providerID, models, remembered)
	if e != nil {
		return e
	}
	if notice != "" {
		fmt.Fprintln(os.Stderr, "ttc:", notice)
	}
	// Persist the startup choice before a writable load can record its switch.
	// Loading itself (validation, copying and activation) is owned by the runtime.
	var source history.Session
	if opts.load != "" {
		source, e = store.Session(opts.load)
		if e != nil {
			return e
		}
		// Reject another workspace before persisting a startup preference. The
		// runtime also validates its load boundary before preparing a snapshot.
		if source.WorkspaceID != "" {
			path, err := store.WorkspacePath(ctx, source.WorkspaceID)
			if err != nil {
				return err
			}
			if path != w.Root {
				return errors.New("session belongs to another workspace")
			}
		}
	}
	if !source.ReadOnly {
		if e := store.SaveSelection(selection); e != nil {
			return e
		}
	}
	// Maintenance owns the database before any runtime or frontend interaction.
	// Its synchronous callbacks write startup stderr, never runtime events.
	notices, e := maintainHistory(ctx, store, opts.load, progress)
	if e != nil {
		return e
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
	runtime, e := session.New(ctx, store, w, p, selection, "", catalog, searchConfig, func(event session.Event) {
		select {
		case events <- event:
		case <-ctx.Done():
		}
	})
	if e != nil {
		return e
	}
	defer func() {
		// Stop UI delivery before joining callbacks after the frontend has exited.
		cancel()
		runtime.Close()
	}()
	rememberedEasy, e := store.LastEasySelection(providerID)
	if e != nil {
		return fmt.Errorf("read saved easy model choice: %w", e)
	}
	if rememberedEasy != nil {
		easy, err := llm.Resolve(providerID, models, rememberedEasy.Model.ID, rememberedEasy.Variant)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ttc: Saved easy model or variant unavailable; using the main model. Use /model to choose.")
		} else if err := runtime.RestoreEasyModel(easy); err != nil {
			return fmt.Errorf("restore easy model choice: %w", err)
		}
	}
	if opts.load != "" {
		if e := runtime.Load(opts.load); e != nil {
			return e
		}
	}
	runtime.AutoName = opts.script == ""
	for _, notice := range notices {
		events <- session.Event{Kind: "status", Text: notice, Generation: runtime.Generation(), SessionID: runtime.Current()}
	}
	ui := &tui.Frontend{Runtime: runtime, Events: events, Input: os.Stdin, Output: os.Stdout, Plain: opts.plain || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())), Login: loginCurrent, Models: models, ModelUpdates: modelUpdates}
	return ui.Run(ctx)
}
