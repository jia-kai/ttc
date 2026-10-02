# MathJax dependencies

## Setup and cache

- TTC pins MathJax **4.1.3** with matching New Computer Modern SVG fonts, the
  [official release](https://github.com/mathjax/MathJax-src/releases/tag/4.1.3)
  verified on 2026-10-01. Dependencies stay in cache, not the executable.
- Embed only the helper, [manifest](../internal/assets/mathjax-package.json) and
  [lockfile](../internal/assets/mathjax-package-lock.json). The lock freezes all 17
  packages with exact versions/URLs/SHA-512 integrity. [npm ci](https://docs.npmjs.com/cli/v11/commands/npm-ci/)
  verifies downloads without modifying metadata; scripts/audit/funding are disabled.
- Install host Node and `rsvg-convert`; npm is needed for initial setup. Initialize
  under `$XDG_CACHE_HOME/ttc/mathjax` (default `~/.cache/ttc/mathjax`), with 0700
  version/lock-addressed roots and npm cache in that hierarchy. Ready roots need
  no npm/network.
- A cancelable lock and two-minute deadline protect staging, validation render
  and atomic publication. Failure preserves old cache; damaged metadata triggers
  staged rebuild. Unsafe permissions/symlinks fail explicitly.

Prepare without login/UI:

```sh
make build
./ttc --install-math
```

## Rendering

- Math initializes independently of images. One pre-warmed Node process serves
  visible formulas, with fresh parser/document state and reused output engine/
  local fonts. Base TeX, AMS and local macros are enabled; dynamic loading is not.
  Remove Node hooks and use absolute cached imports. Join interrupted/broken workers
  and restart on the next render; ordinary TeX errors keep the engine warm.
- Conversation/Markdown windows share asset work. Failure warns and shows labeled
  literal TeX; PNGs use a separate pruned render cache. Cached PNGs bypass engines.

| Limit                        | Value                 |
| ---------------------------- | --------------------- |
| Formula source               | 4096 bytes            |
| One render deadline          | 10 seconds            |
| JSON-line reply/SVG/PNG      | 32 MiB each           |
| Captured worker stderr       | 4 KiB                 |
| Logical dimensions           | 4096×1024 pixels      |
| Physical canvas              | 16 megapixels         |
| Retained decoded image pool  | 32 MiB, images + math |

- Rasterize at three pixels per logical pixel on both axes, including padding;
  retain full rasters in cache. Uniformly fit uploads to logical/placement limits
  without enlarging images, then average source area in premultiplied sRGB/alpha.
  Transparent padding fills the rounded terminal-cell grid without stretching;
  SVG padding preserves em size. No hinting or RGB subpixel mode.
- Filter/upload once per visible placement size; redraw reuses it. Placement IDs
  include measured cell size, so font-size changes upload again. Missing measurements
  warn and estimate 8×16 pixels/cell. Kitty detection enables RGB without `COLORTERM`;
  `NO_COLOR` or `TCELL_TRUECOLOR=disable` disables graphics and warns on formulas.
- Render keys include resolution/backend hash. Check physical dimensions before
  conversion. Decoded replies transfer directly to UI; fitted pixels/cell-aligned
  upload canvases are temporary allocations outside its retained image pool.

## Updating and measuring

- Update the tracked manifest and Go version constant; regenerate the lockfile
  with npm in private cache and review every URL/integrity change. Prepare the
  cache, then run unit/real math/Kitty checks. Runtime never resolves unpinned latest.
- Benchmark the warm backend plus PNG conversion, excluding initial downloads:

```sh
go test ./internal/assets -run '^$' -bench BenchmarkMathJax -benchtime=10x
```
