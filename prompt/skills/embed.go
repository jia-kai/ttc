// Package bundledskills embeds the bundled tool-use recipes in the binary.
package bundledskills

import "embed"

// Files contains bundled SKILL.md documents, independent of the working directory.
//
//go:embed */SKILL.md
var Files embed.FS
