// Package providers composes provider-specific dependencies without owning
// catalog workers, authorization presentation, or inference lifecycle policy.
package providers

import (
	"context"
	"errors"
	"fmt"
	"github.com/spf13/pflag"
	"net/http"

	"ttc/internal/auth"
	"ttc/internal/catalog"
	"ttc/internal/llm"
)

// Module declares a provider and its flag configuration. Configure must register
// flags only on the supplied flag set; each call must create independent state.
type Module struct {
	ID        string
	Configure func(*pflag.FlagSet) (Factory, error)
}

// Factory constructs components using parsed configuration. Constructors own no
// workers or resources requiring closure; callers own subsequent operations.
type Factory func(context.Context, Environment) (Components, error)

// Environment supplies application-owned dependencies for construction.
type Environment struct {
	DataDir string
	// Client is optional; nil selects the provider's default HTTP client.
	Client *http.Client
	// ResolveBinary resolves original attachments on demand, not at construction.
	ResolveBinary func(context.Context, llm.BinaryFile) (llm.BinaryPayload, error)
}

// Components separates operational dependencies. Inference is required;
// Catalog and Authorize are optional. Notice is a non-secret startup message.
type Components struct {
	Inference Inference
	Catalog   *Catalog
	Authorize func(context.Context, auth.UI) error
	Notice    string
}

// Inference is the operational dependency returned by a module. Defining it
// here keeps composition independent of the session implementation; compatible
// runtime interfaces accept it structurally without adapters.
type Inference interface {
	Stream(context.Context, llm.Request, func(llm.StreamEvent) error) error
	// Cancel best-effort cancels only the attempt identified by the remote
	// response ID, never a conversation or the provider's latest request. An
	// empty ID means no remote identity is available; backends unable to address
	// that attempt must no-op. Stream owns invocation on abandonment, after
	// immediate local abort, with a fresh bounded context and before retrying.
	// Implementations must honor the context and support concurrent attempts.
	// Unsupported remote cancellation is a no-op, never a local-abort substitute.
	Cancel(context.Context, string) error
	EstimateReplay(llm.Message) int
	TokenizerID(llm.ModelSpec) string
}

// Catalog supplies discovery binding and a cache location, not refresh policy.
// The caller owns the catalog manager, deadlines, workers and shutdown.
type Catalog struct {
	Bind      func(context.Context) (catalog.Binding, error)
	CachePath string
}

// Configured holds a provider's factory backed by its registered flag values.
// Records returned by Configure retain flag ownership for Select validation.
type Configured struct {
	ID         string
	Open       Factory
	flagSet    *pflag.FlagSet
	ownedFlags map[string]bool
}

// Configure stages all module flags before adding them to fs. Invalid modules,
// configuration failures and flag collisions leave fs unchanged. Merged flags
// retain the original Value pointers so parsing updates the factories' state.
func Configure(modules []Module, fs *pflag.FlagSet) ([]Configured, error) {
	if fs == nil {
		return nil, errors.New("provider configuration requires a flag set")
	}
	if len(modules) == 0 {
		return nil, errors.New("at least one provider module is required")
	}
	ids := make(map[string]bool, len(modules))
	for _, module := range modules {
		if err := llm.ValidateChoiceText("provider module ID", module.ID, llm.MaxProviderIDBytes); err != nil {
			return nil, err
		}
		if ids[module.ID] {
			return nil, fmt.Errorf("duplicate provider module %q", module.ID)
		}
		ids[module.ID] = true
		if module.Configure == nil {
			return nil, fmt.Errorf("provider %q has no configuration function", module.ID)
		}
	}
	var flags []*pflag.Flag
	names := make(map[string]bool)
	shorthands := make(map[string]bool)
	configured := make([]Configured, 0, len(modules))
	for _, module := range modules {
		staged := pflag.NewFlagSet(module.ID, pflag.ContinueOnError)
		staged.SetOutput(fs.Output())
		factory, err := configureModule(module, staged)
		if err != nil {
			return nil, fmt.Errorf("configure provider %q: %w", module.ID, err)
		}
		if factory == nil {
			return nil, fmt.Errorf("provider %q returned no factory", module.ID)
		}
		ownedFlags := make(map[string]bool)
		staged.VisitAll(func(f *pflag.Flag) {
			if names[f.Name] || fs.Lookup(f.Name) != nil {
				err = fmt.Errorf("provider flag %q is already registered", f.Name)
			}
			if f.Shorthand != "" {
				if shorthands[f.Shorthand] || fs.ShorthandLookup(f.Shorthand) != nil {
					err = fmt.Errorf("provider flag shorthand %q is already registered", f.Shorthand)
				}
				shorthands[f.Shorthand] = true
			}
			names[f.Name] = true
			ownedFlags[f.Name] = true
			flags = append(flags, f)
		})
		if err != nil {
			return nil, err
		}
		configured = append(configured, Configured{ID: module.ID, Open: factory, flagSet: fs, ownedFlags: ownedFlags})
	}
	for _, f := range flags {
		fs.AddFlag(f)
	}
	return configured, nil
}

// pflag panics on invalid or duplicate declarations. Keep those failures inside
// the staging boundary rather than crashing the CLI or partially registering.
func configureModule(module Module, fs *pflag.FlagSet) (factory Factory, err error) {
	defer func() {
		if failure := recover(); failure != nil {
			err = fmt.Errorf("invalid flag configuration: %v", failure)
		}
	}()
	return module.Configure(fs)
}

// Select validates configured providers and returns the explicitly requested
// factory. Callers choose their default ID; an empty or unknown ID is an error.
// Call after parsing: records returned by Configure reject explicitly supplied
// flags owned by nonselected modules, including values equal to their defaults.
// Application flags and unset module defaults do not affect selection. Manually
// constructed records have no flag ownership metadata to validate.
func Select(configured []Configured, id string) (Factory, error) {
	if err := llm.ValidateChoiceText("provider ID", id, llm.MaxProviderIDBytes); err != nil {
		return nil, err
	}
	if len(configured) == 0 {
		return nil, errors.New("at least one configured provider is required")
	}
	ids := make(map[string]bool, len(configured))
	var selected Factory
	for _, item := range configured {
		if err := llm.ValidateChoiceText("configured provider ID", item.ID, llm.MaxProviderIDBytes); err != nil {
			return nil, err
		}
		if item.Open == nil {
			return nil, fmt.Errorf("configured provider %q requires a factory", item.ID)
		}
		if ids[item.ID] {
			return nil, fmt.Errorf("duplicate configured provider %q", item.ID)
		}
		ids[item.ID] = true
		if item.ID == id {
			selected = item.Open
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("unknown provider %q", id)
	}
	for _, item := range configured {
		if item.ID == id || item.flagSet == nil {
			continue
		}
		var err error
		item.flagSet.Visit(func(f *pflag.Flag) {
			if err == nil && item.ownedFlags[f.Name] {
				err = fmt.Errorf("flag --%s belongs to provider %q, not selected provider %q", f.Name, item.ID, id)
			}
		})
		if err != nil {
			return nil, err
		}
	}
	return selected, nil
}
