package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ttc/internal/auth"
	"ttc/internal/catalog"
	"ttc/internal/history"
	"ttc/internal/llm"
	"ttc/internal/providers"
)

// The fixture crosses the subprocess boundary explicitly, including expected
// errors: a child exiting successfully without checking run's error is not a pass.
type moduleCLIFixture struct {
	Mode, Data, Work, Input, WantError, WantProvider, WantModel string
	Args                                                        []string
	WantCalls                                                   [4]int // factory, bind, discovery, authorize
}

type moduleCLISource func(context.Context) ([]llm.ModelInfo, error)

func (s moduleCLISource) Models(ctx context.Context) ([]llm.ModelInfo, error) { return s(ctx) }

func runModuleCLIFixture(t *testing.T, f moduleCLIFixture) {
	t.Helper()
	var calls [4]int
	authorized := false
	module := func(id string) providers.Module {
		return providers.Module{ID: id, Configure: func(fs *flag.FlagSet) (providers.Factory, error) {
			value := fs.String(id+"-value", "default", "fixture-owned string")
			count := fs.Int(id+"-count", 7, "fixture-owned integer")
			enabled := fs.Bool(id+"-enabled", false, "fixture-owned boolean")
			return func(ctx context.Context, env providers.Environment) (providers.Components, error) {
				calls[0]++
				if strings.HasPrefix(f.Mode, "offline") || f.WantCalls[0] == 0 {
					t.Fatal("provider factory called before validation or in offline mode")
				}
				if id != f.WantProvider || env.DataDir != f.Data || env.ResolveBinary == nil || ctx.Err() != nil {
					t.Fatalf("wrong factory/environment: id=%q env=%+v ctx=%v", id, env, ctx.Err())
				}
				if f.Mode == "flags" && (*value != "configured" || *count != 19 || !*enabled) {
					t.Fatalf("parsed module flags did not reach factory: %q %d %v", *value, *count, *enabled)
				}
				components := providers.Components{Inference: &llm.Script{}}
				components.Catalog = &providers.Catalog{CachePath: filepath.Join(env.DataDir, id+"-models.json"), Bind: func(ctx context.Context) (catalog.Binding, error) {
					calls[1]++
					if f.Mode == "login" {
						t.Fatal("--login reached catalog binding")
					}
					scopeID := id
					if f.Mode == "scope" {
						scopeID = "foreign"
					}
					fresh := authorized
					return catalog.Binding{Scope: catalog.Scope{Provider: scopeID, Endpoint: "fixture", Version: "v1"}, Source: moduleCLISource(func(ctx context.Context) ([]llm.ModelInfo, error) {
						calls[2]++
						if err := ctx.Err(); err != nil {
							return nil, err
						}
						model := "old"
						if fresh {
							model = "fresh"
							// Reauthorization must discover a new binding, not reuse
							// the old cache, and must not silently replace the choice.
							assertModuleCLISelection(t, f.Data, id, "old")
						}
						return []llm.ModelInfo{{ID: model, Name: "Fixture " + model, Variants: []string{"none"}, DefaultVariant: "none", Limits: llm.ModelLimits{ContextLimit: 100000, MaxOutputTokens: 8192}, Revision: model}}, nil
					})}, nil
				}}
				components.Authorize = func(ctx context.Context, ui auth.UI) error {
					calls[3]++
					if f.Mode != "login" {
						assertModuleCLISelection(t, f.Data, id, "old")
					}
					authorized = true
					return ui.Present(ctx, auth.Step{Message: "Fixture authorization complete"})
				}
				switch f.Mode {
				case "nil-inference":
					components.Inference = nil
				case "nil-catalog":
					components.Catalog = nil
				case "nil-bind":
					components.Catalog.Bind = nil
				case "nil-auth":
					components.Authorize = nil
				}
				return components, nil
			}, nil
		}}
	}
	modules := []providers.Module{module("first"), module("second")}
	switch f.Mode {
	case "default-second":
		modules[0], modules[1] = modules[1], modules[0]
	case "duplicate":
		modules[1].ID = modules[0].ID
	case "nil-config":
		modules[0].Configure = nil
	case "missing-factory":
		modules[0].Configure = func(*flag.FlagSet) (providers.Factory, error) { return nil, nil }
	case "config-error":
		modules[0].Configure = func(*flag.FlagSet) (providers.Factory, error) { return nil, errors.New("fixture configuration failed") }
	case "app-flag", "module-flag":
		name := "provider"
		if f.Mode == "module-flag" {
			name = "first-value"
		}
		modules[1].Configure = func(fs *flag.FlagSet) (providers.Factory, error) {
			fs.String(name, "", "conflicting fixture flag")
			return func(context.Context, providers.Environment) (providers.Components, error) {
				t.Fatal("conflicting factory invoked")
				return providers.Components{}, nil
			}, nil
		}
	}
	flag.CommandLine = flag.NewFlagSet("ttc", flag.ContinueOnError)
	os.Args = append([]string{"ttc", "--plain", "--data-dir", f.Data, "--workdir", f.Work}, f.Args...)
	var err error
	if f.Mode == "production-script" {
		err = run()
	} else {
		err = runWithModules(modules)
	}
	if f.WantError != "" {
		if err == nil || !strings.Contains(err.Error(), f.WantError) {
			t.Fatalf("run error=%v; want %q", err, f.WantError)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if calls != f.WantCalls {
		t.Fatalf("component calls=%v; want %v", calls, f.WantCalls)
	}
}

func assertModuleCLISelection(t *testing.T, data, id, model string) {
	t.Helper()
	store, err := history.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	selection, readErr := store.LastSelection(id)
	closeErr := store.Close()
	if readErr != nil || closeErr != nil || selection == nil || selection.Provider != id || selection.Model.ID != model || selection.Variant != "none" {
		t.Fatalf("saved choice=%+v; want %s/%s/none; read=%v close=%v", selection, id, model, readErr, closeErr)
	}
}

func TestProviderModulesCLI(t *testing.T) {
	if raw := os.Getenv("TTC_TEST_MODULE_FIXTURE"); raw != "" {
		var fixture moduleCLIFixture
		if err := json.Unmarshal([]byte(raw), &fixture); err != nil {
			t.Fatal(err)
		}
		runModuleCLIFixture(t, fixture)
		return
	}
	fixtures := []moduleCLIFixture{
		{Mode: "default", WantProvider: "first", WantModel: "old", WantCalls: [4]int{1, 1, 1, 0}},
		{Mode: "default-second", WantProvider: "second", WantModel: "old", WantCalls: [4]int{1, 1, 1, 0}},
		{Mode: "flags", Args: []string{"--provider", "second", "--second-value", "configured", "--second-count", "19", "--second-enabled"}, WantProvider: "second", WantModel: "old", WantCalls: [4]int{1, 1, 1, 0}},
		{Mode: "login", Args: []string{"--provider", "second", "--login"}, WantProvider: "second", WantCalls: [4]int{1, 0, 0, 1}},
		{Mode: "interactive-login", Input: "/login\n/model\n/quit\n", WantProvider: "first", WantModel: "old", WantCalls: [4]int{1, 2, 2, 1}},
		{Mode: "interactive-reselect", Input: "/login\n/model fresh none\n/quit\n", WantProvider: "first", WantModel: "fresh", WantCalls: [4]int{1, 2, 2, 1}},
		{Mode: "nil-inference", WantProvider: "first", WantError: `provider "first" has no inference component`, WantCalls: [4]int{1, 0, 0, 0}},
		{Mode: "nil-catalog", WantProvider: "first", WantError: `provider "first" has no model catalog`, WantCalls: [4]int{1, 0, 0, 0}},
		{Mode: "nil-bind", WantProvider: "first", WantError: `provider "first" has no model catalog`, WantCalls: [4]int{1, 0, 0, 0}},
		{Mode: "nil-auth", Args: []string{"--login"}, WantProvider: "first", WantError: `provider "first" does not support authorization`, WantCalls: [4]int{1, 0, 0, 0}},
		{Mode: "scope", WantProvider: "first", WantError: `provider "first" returned catalog scope "foreign"`, WantCalls: [4]int{1, 1, 0, 0}},
		{Mode: "unknown", Args: []string{"--provider", "unknown"}, WantError: `unknown provider "unknown"`},
		{Mode: "unselected-flag", Args: []string{"--second-value", "default"}, WantError: `flag --second-value belongs to provider "second", not selected provider "first"`},
		{Mode: "production-script", Args: []string{"--provider", "script"}, WantError: `unknown provider "script"`},
		{Mode: "duplicate", WantError: `duplicate provider module "first"`},
		{Mode: "nil-config", WantError: `provider "first" has no configuration function`},
		{Mode: "missing-factory", WantError: `provider "first" returned no factory`},
		{Mode: "config-error", WantError: "fixture configuration failed"},
		{Mode: "app-flag", WantError: `provider flag "provider" is already registered`},
		{Mode: "module-flag", WantError: `provider flag "first-value" is already registered`},
		{Mode: "offline", WantProvider: "script", WantModel: "scripted"},
		{Mode: "offline-provider", Args: []string{"--provider", "first"}, WantError: "--offline-script cannot be combined"},
		{Mode: "offline-flag", Args: []string{"--first-value", "default"}, WantError: "--offline-script cannot be combined"},
		{Mode: "offline-bool", Args: []string{"--first-enabled=false"}, WantError: "--offline-script cannot be combined"},
		{Mode: "offline-login", Args: []string{"--login"}, WantError: "--offline-script cannot be combined"},
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Mode, func(t *testing.T) {
			if fixture.WantCalls[0] > 0 || fixture.Mode == "offline" {
				if _, err := exec.LookPath("rg"); err != nil {
					t.Skip("CLI runtime requires ripgrep")
				}
			}
			root := t.TempDir()
			fixture.Data, fixture.Work = filepath.Join(root, "data"), filepath.Join(root, "work")
			if err := os.Mkdir(fixture.Work, 0700); err != nil {
				t.Fatal(err)
			}
			if fixture.Input == "" {
				fixture.Input = "/quit\n"
			}
			if strings.HasPrefix(fixture.Mode, "offline") {
				script := filepath.Join(root, "responses.json")
				if err := os.WriteFile(script, []byte("[]"), 0600); err != nil {
					t.Fatal(err)
				}
				fixture.Args = append(fixture.Args, "--offline-script", script)
			}
			raw, err := json.Marshal(fixture)
			if err != nil {
				t.Fatal(err)
			}
			// Save fixture context before running; retain diagnostics outside
			// t.TempDir on failure so test cleanup cannot erase the evidence.
			t.Cleanup(func() {
				if !t.Failed() {
					return
				}
				retained, err := os.MkdirTemp("", "ttc-module-cli-failure-")
				if err != nil {
					t.Logf("retain diagnostics: %v", err)
					return
				}
				for _, name := range []string{"fixture.json", "output.log"} {
					contents, err := os.ReadFile(filepath.Join(root, name))
					if err == nil {
						err = os.WriteFile(filepath.Join(retained, name), contents, 0600)
					}
					if err != nil {
						t.Logf("retain %s: %v", name, err)
					}
				}
				t.Logf("module CLI failure diagnostics: %s", retained)
			})
			if err := os.WriteFile(filepath.Join(root, "fixture.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestProviderModulesCLI$")
			for _, item := range os.Environ() {
				name, _, _ := strings.Cut(item, "=")
				if strings.HasPrefix(name, "TTC_") || name == "HOME" || strings.HasPrefix(name, "XDG_") {
					continue
				}
				cmd.Env = append(cmd.Env, item)
			}
			cmd.Env = append(cmd.Env, "TTC_TEST_MODULE_FIXTURE="+string(raw), "HOME="+root, "XDG_CONFIG_HOME="+root, "XDG_CACHE_HOME="+root)
			cmd.Stdin = strings.NewReader(fixture.Input)
			output, runErr := cmd.CombinedOutput()
			if err := os.WriteFile(filepath.Join(root, "output.log"), output, 0600); err != nil {
				t.Fatal(err)
			}
			if runErr != nil {
				t.Fatalf("module CLI: %v\nfixture: %s\n%s", runErr, raw, output)
			}
			if fixture.WantModel != "" {
				assertModuleCLISelection(t, fixture.Data, fixture.WantProvider, fixture.WantModel)
				if !strings.Contains(string(output), "TTC · Linux terminal agent") {
					t.Fatalf("frontend did not start: %s", output)
				}
			}
			if strings.HasPrefix(fixture.Mode, "interactive") {
				for _, want := range []string{"Fixture authorization complete", "Login complete"} {
					if !strings.Contains(string(output), want) {
						t.Fatalf("missing %q: %s", want, output)
					}
				}
				if fixture.Mode == "interactive-login" && !strings.Contains(string(output), "fresh · Fixture fresh") {
					t.Fatalf("picker did not receive refreshed catalog: %s", output)
				}
				if fixture.Mode == "interactive-reselect" && !strings.Contains(string(output), "Model switched · fresh · none") {
					t.Fatalf("explicit model switch missing: %s", output)
				}
			}
			if fixture.Mode == "login" && (!strings.Contains(string(output), "Fixture authorization complete") || strings.Contains(string(output), "TTC · Linux terminal agent")) {
				t.Fatalf("login did not exit before frontend: %s", output)
			}
		})
	}
}
