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
the next render; ordinary TeX errors preserve the warm engine. Unsupported input
retains literal TeX. PNGs remain in the separate pruned render cache.

To update dependencies, change the exact version in the tracked manifest and Go
constant, regenerate the lockfile with npm in a private cache directory, then
review every resolved URL/integrity change. Run unit tests, real math tests and
Kitty visual checks after preparing the updated cache. The runtime never resolves
an unpinned latest version automatically.

Fresh-process Node+PNG samples on the development host (Node 26.5.1,
librsvg 2.62.3) measured 215–247 ms before warming. Ten-render warm benchmarks
averaged 21.7 ms for inline math, 22.5 ms for a matrix and 28.1 ms for aligned
equations, including PNG conversion. Benchmark the actual warm renderer with:

```sh
go test ./internal/assets -run '^$' -bench BenchmarkMathJax -benchtime=10x
```

These timings exclude first-time npm downloads. Cached PNGs bypass both engines.
