// Package openai owns ChatGPT subscription authorization, private credentials,
// model discovery and the Responses protocol. Module composes those capabilities
// without owning application catalog workers or inference lifecycle policy.
package openai

import (
	"context"
	"errors"
	"flag"
	"path/filepath"
	"strings"

	"ttc/internal/catalog"
	"ttc/internal/providers"
)

// Module returns independent OpenAI configuration and component construction.
// Construction accesses the credential store only for an explicit Codex import.
func Module() providers.Module {
	return providers.Module{ID: "openai", Configure: func(fs *flag.FlagSet) (providers.Factory, error) {
		endpoint := fs.String("openai-base-url", "", "explicit subscription endpoint override for local mock-server tests")
		importAuth := fs.String("import-codex-auth", "", "explicitly copy ChatGPT subscription credentials from a file")
		return func(ctx context.Context, env providers.Environment) (providers.Components, error) {
			if ctx == nil {
				return providers.Components{}, errors.New("provider construction requires a context")
			}
			if err := ctx.Err(); err != nil {
				return providers.Components{}, err
			}
			if strings.TrimSpace(env.DataDir) == "" {
				return providers.Components{}, errors.New("OpenAI provider requires a data directory")
			}
			owner := NewAuthenticator(filepath.Join(env.DataDir, "openai-auth.json"))
			if env.Client != nil {
				owner.Client = env.Client
			}
			transport := NewAdapter(Config{Client: env.Client, BaseURL: *endpoint, TokenSource: owner.AccessTokens, ResolveBinary: env.ResolveBinary})
			components := providers.Components{
				Inference: transport,
				Catalog: &providers.Catalog{Bind: func(ctx context.Context) (catalog.Binding, error) {
					return bindCatalog(ctx, owner, transport)
				}, CachePath: filepath.Join(env.DataDir, "openai-models.json")},
				Authorize: owner.Login,
			}
			if *importAuth != "" {
				if err := owner.ImportCodex(ctx, *importAuth); err != nil {
					return providers.Components{}, err
				}
				components.Notice = "Imported subscription credentials into private TTC storage"
			}
			return components, nil
		}, nil
	}}
}

// bindCatalog captures local identity without refreshing credentials. The
// discovery transport may acquire fresh tokens, but cannot switch the account
// behind the captured scope. Publication holds the shared credential writer lock.
func bindCatalog(ctx context.Context, owner *Authenticator, transport *Adapter) (catalog.Binding, error) {
	current, err := owner.Current(ctx)
	if err != nil {
		return catalog.Binding{}, err
	}
	account := current.Tokens.AccountID
	source := NewAdapter(Config{Client: transport.Client, BaseURL: transport.BaseURL, TokenSource: func(ctx context.Context) (AccessTokens, error) {
		tokens, err := owner.AccessTokens(ctx)
		if err != nil {
			return AccessTokens{}, err
		}
		if tokens.AccountID != account {
			return AccessTokens{}, errors.New("subscription account changed before model catalog request")
		}
		return tokens, nil
	}})
	return catalog.Binding{
		Scope:    catalog.Scope{Provider: "openai", Endpoint: source.BaseURL, Account: account, Version: CatalogVersion},
		Source:   source,
		Guard:    func(ctx context.Context, commit func() error) error { return owner.WithAccount(ctx, account, commit) },
		Validate: ValidateCatalog,
	}, nil
}
