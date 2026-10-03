# MathJax dependencies

## Setup and cache

- Pin MathJax [4.1.3](https://github.com/mathjax/MathJax-src/releases/tag/4.1.3)
  and matching New Computer Modern SVG fonts. Embed only the helper,
  [manifest](../internal/assets/mathjax-package.json) and
  [lockfile](../internal/assets/mathjax-package-lock.json), not dependencies.
  The lock pins 17 packages by version/URL/SHA-512; [npm ci](https://docs.npmjs.com/cli/v11/commands/npm-ci/)
  verifies downloads without changing metadata. Disable scripts/audit/funding.
- Install host Node and `rsvg-convert`; npm is needed only for setup. On Arch:
  `sudo pacman -S nodejs npm librsvg`. Use 0700 version/lock-addressed roots and npm cache under `$XDG_CACHE_HOME/ttc/mathjax`
  (default `~/.cache/ttc/mathjax`). Ready roots need no npm/network.
- A cancelable lock and two-minute deadline bound staging, validation render and
  publication. Stage/validate before replacing the cache; damaged metadata triggers
  rebuild. Unsafe permissions/symlinks fail explicitly.
- Prepare without login/UI:

```sh
make build
./ttc --install-math
```

## Rendering

- Initialize independently of images. One pre-warmed Node serves visible formulas:
  fresh parser/document state, reused output engine/local fonts. Enable base TeX,
  AMS and local macros, not dynamic loading; strip Node hooks and import absolute
  cached paths. Join broken/interrupted workers; restart on next render. Ordinary
  TeX errors leave the engine warm.
- Conversation/Markdown windows share asset work. Failure warns and shows labeled
  literal TeX. Cached PNGs bypass engines; [shared image/math asset ownership](design.md#image-and-math-assets)
  defines cache pruning and retained-pixel budgets.

| Limit                       | Value                 |
| --------------------------- | --------------------- |
| Formula source              | 1–4096 bytes          |
| Render deadline             | 10 seconds            |
| JSON-line reply/SVG/PNG     | 32 MiB each           |
| Captured worker stderr      | 4 KiB                 |
| Logical dimensions          | 4096×1024 pixels      |
| Physical canvas             | 16×1024² pixels       |

- Rasterize at 3× per axis, padding included; cache full rasters. Uniformly fit to
  logical/placement limits without enlargement, averaging source area in
  premultiplied sRGB/alpha. Transparent padding rounds to the cell grid without
  stretching; SVG padding preserves em size. No hinting/RGB subpixel mode.
- Filter/upload once per visible placement size; reuse on redraw. Placement IDs
  include measured cell size, so font-size changes reupload. Missing measurements
  warn and estimate 8×16 pixels/cell. Kitty enables RGB without `COLORTERM`;
  `NO_COLOR` or `TCELL_TRUECOLOR=disable` disables graphics and warns on formulas.
- Render keys include resolution/backend hash; check physical dimensions before
  conversion. Decoded replies transfer to UI; fitted pixels/cell-aligned upload
  canvases are temporary allocations outside the retained pool.

## Updating and measuring

- Update manifest and Go version constant; regenerate the lockfile with npm in
  private cache and review URL/integrity changes. Prepare cache, then run unit,
  real-math and Kitty checks. Runtime never resolves unpinned latest.
- Benchmark warm rendering plus PNG conversion, excluding downloads:

```sh
go test ./internal/assets -run '^$' -bench BenchmarkMathJax -benchtime=10x
```
