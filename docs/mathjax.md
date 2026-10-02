# MathJax dependencies

TTC pins MathJax **4.1.3** and its matching New Computer Modern SVG font package.
It is the [latest official release](https://github.com/mathjax/MathJax-src/releases/tag/4.1.3)
verified on 2026-10-01. Packages remain in the user's cache; their archives and
JavaScript libraries are not embedded in the executable.

The executable embeds only its small rendering helper and the tracked
[package manifest](../internal/assets/mathjax-package.json) and
[lockfile](../internal/assets/mathjax-package-lock.json). The lockfile freezes all
17 downloaded packages with exact versions, resolved URLs and SHA-512 integrity
values. [npm ci](https://docs.npmjs.com/cli/v11/commands/npm-ci/) installs that tree
without changing the manifest or lock; npm verifies the download integrity.
Package scripts, audits and funding notices are disabled.

Node and `rsvg-convert` are host prerequisites. Renderer initialization installs
missing packages using npm under `$XDG_CACHE_HOME/ttc/mathjax`, or
`~/.cache/ttc/mathjax` by default. Installed roots have mode 0700 and contain a
version/lock hash; npm's download cache stays in that same cache hierarchy.
Installation has a cancelable lock and two-minute deadline, stages separately,
renders a validation formula and publishes only a complete package. Failed setup
preserves the previous cache. Damaged metadata triggers a staged rebuild; unsafe
root permissions/symlinks fail explicitly. Ready packages need no npm/network.

To prepare the cache without login or launching the UI:

```sh
make build
./ttc --install-math
```

The UI initializes math independently of image rendering. One pre-warmed Node
process handles visible formulas; parser/document state is fresh per formula,
while the output engine and local font chunks are reused. No dynamic TeX package
loading is enabled. Interrupted or broken workers are joined and restarted on
the next render; ordinary TeX errors preserve the warm engine. Failures show a
warning and labeled literal TeX. Conversation and Markdown inspection windows
share the visible-asset renderer. PNGs remain in the separate pruned render cache.

Formulas rasterize at three pixels per logical display pixel in each direction,
including padding. The render cache retains these full rasters. Before uploading,
TTC uniformly fits the raster to its logical size and placement limits, then
averages source-pixel area in premultiplied sRGB and alpha. Transparent padding
fills the rounded terminal cell grid without stretching glyphs. SVG padding
preserves the configured em size. Filtering and PNG upload run once per visible
placement size; redraws reuse the upload. Images are never enlarged by the
filter. There is no font hinting or RGB subpixel mode.

Placement IDs include the measured cell size; font-size changes trigger a new
upload. Missing measurements produce a UI warning and an estimated 8×16 cell.
Successful Kitty detection enables RGB even if SSH/tmux omits `COLORTERM`.
Explicit `NO_COLOR` or `TCELL_TRUECOLOR=disable` disables graphics and warns on
formula output.
Render keys include raster resolution and backend hash. Physical canvases are
checked against the 16-megapixel limit before conversion. Decoded replies pass
directly to the UI; its retained image budget remains 32 MiB. The filtered upload
temporarily allocates the fitted pixels and cell-aligned canvas, then discards
them.

To update dependencies, change the exact version in the tracked manifest and Go
constant, regenerate the lockfile with npm in a private cache directory, then
review every resolved URL/integrity change. Run unit tests, real math tests and
Kitty visual checks after preparing the updated cache. The runtime never resolves
an unpinned latest version automatically.

Benchmark the actual warm renderer, including PNG conversion, with:

```sh
go test ./internal/assets -run '^$' -bench BenchmarkMathJax -benchtime=10x
```

Benchmarks exclude first-time npm downloads. Cached PNGs bypass both engines.
