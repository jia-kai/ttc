// Package builtin explicitly lists the compiled production provider modules.
package builtin

import (
	"ttc/internal/providers"
	"ttc/internal/providers/openai"
)

// Modules returns a fresh, ordered list of production modules. The first module
// is the application's default. Offline scripting is not a production provider.
func Modules() []providers.Module {
	return []providers.Module{openai.Module()}
}
