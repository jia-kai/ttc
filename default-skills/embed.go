// Package defaultskills embeds the default tool-use recipes in the binary.
package defaultskills

import "embed"

// Files contains bundled SKILL.md documents, independent of the working directory.
//
//go:embed */SKILL.md
var Files embed.FS
