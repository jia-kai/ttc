# Local demonstration

Requirements: Linux, Go 1.26.5+, Python 3, and ripgrep (`rg`). No OpenAI login,
network service, subscription usage, or Python packages are needed. The mock
OpenAI server listens only on loopback and uses synthetic credentials. It
checks requests and sends a predefined Responses/SSE conversation.

For visual inspection, run this inside your terminal or a tmux pane:

```sh
make demo
```

Type `Run demo` and press Enter. The question dialog has three question tabs
and a final Submit tab. Prompts and options form one block, with controls below;
the review numbers questions to match their tabs. Select recommended **Yes** with Enter to advance,
choose **Other**, and enter `Looks good.`. Press Enter to finish text, then
Right to the checks. Select **Data** with Enter to advance to **Submit**, and
press Enter again to send the complete round. Each question is single-choice;
Space selects without advancing.
Left/Right preserves answers while switching tabs; nothing submits until the
final button. Esc dismisses; `/questions` reopens the pending dialog. Sending a
normal message instead completes dismissed calls with `dismissed:true` and
redirects the agent; local commands leave them pending.

The fixture exercises all 20
implemented tool types: file read/search,
three edit types, shell, a reusable child with an idle follow-up and explicit close, a managed stdio LSP
query, background job list/read/stop, local web
fetch/search, skill loading, a saved image and its dimensions, questions, and three timer tools.
The final notebook demonstrates headings, tables, emphasis, quotes, nested and
ordered lists, checkboxes, code, links, Unicode, and inline/block TeX math.
Kitty displays images and formula images with Unicode placeholders; unsupported
TeX stays readable. Click the field thumbnail for pan/zoom and coordinate selection.
In a graphics-capable interactive demo, OK sends the requested point to the mock
server; Esc sends cancellation. Ctrl+X F toggles fullscreen copy mode, hiding conversation scroll decorations
and disabling mouse capture; select/copy with the terminal and scroll by
Ctrl+U/D. The display freezes while runtime work continues; periodic redraws and
automatic question dialogs pause until exit. Esc or Ctrl+X F restores normal mouse behavior. Ctrl+X S opens the
sidebar overlay; click headers to expand lists and wheel-scroll each independently.

Inspect any message/tool by clicking its row, or use Alt+Up/Down to focus a row and
Tab on an empty composer. The system-prompt placeholder opens the exact prompt
used by that actor. Each actor has one prompt placeholder showing its latest
snapshot; exact snapshots are retained for every request without adding model calls. Tool briefings normally use one
clipped row with short arguments/status and labeled output excerpts; clicking
opens full saved details with readable parameters/results. File mutations add
short diff previews, with path headers and highlighted diffs in inspectors.
Glob shows only its pattern. Generated IDs use readable
prefixes and a 16-character URL-safe random suffix.
Inside the shared display window, use Page Up/Down, Ctrl+U/D,
Home/End, and Esc. Up/Down in the composer recalls prompts and restores drafts.
Ctrl+U/D scrolls the conversation; Ctrl+D at the bottom follows new output and never exits. Ctrl+C exits from any
view. Try `/model` or Ctrl+X,
then M to browse model families and variants; Esc cancels. Keep Demo Sol/low
selected while running the fixed interactive script. After completion you can
switch models freely, open `/sessions` to select/reload a session, inspect history, and quit with `/quit`.

Each invocation makes a fresh private project copied from
`tests/fixtures/research` under `/tmp/ttc/<uid>/demo-*`. It prints that
directory. The original fixture is never modified. Requests, transcript,
coverage, history, and failure context are retained for automated runs;
interactive runs retain requests, project, and history. Restart `make demo`
to reset the predefined conversation.
The shared `/tmp/ttc` root is sticky 1777; each numeric UID directory is owned
by that user and private (0700). Test drivers reject unsafe existing paths.

For reproducible headless validation:

```sh
make full-test                    # all automated suites, including graphics/math
make integration                  # original workflow + plain and TUI demos
python3 tests/demo.py              # automated plain PTY
python3 tests/demo.py --tui        # real TUI in a PTY; switches model via menu
python3 tests/pty_input.py          # offline editor/completion/export, Ctrl+J and /rename
python3 tests/pty_history.py        # offline prompt recall/search across restart and /new
python3 tests/pty_parallel.py       # shared data/workspace, independent loads, no recovery
python3 tests/pty_compaction.py     # offline context handoff and ancestor discovery
python3 tests/pty_compaction_reload.py # offline CLI/TUI: boundary reload and automatic pending delivery
python3 tests/pty_subagent.py       # mock HTTP: disposable child, low variant, direct answer
python3 tests/pty_subagent.py --offline # socket-free foreground disposal and UTF-8 answer limit
python3 -m unittest discover -s tests -p test_scratch.py # scratch permissions
make check                        # race tests and vet
make rail-integration             # real filesystem sandbox and tmux lifecycle
```

`make full-test` runs Go tests/race/vet, PTY and rail regressions, Python unit
tests, all direct/tmux and mock/offline Kitty modes, and math protocol tests with
both explicit color-disable settings. Suites run sequentially even with `-j`;
the first failure stops the run. Manual demos and benchmarks are excluded.
Run it outside rail if `./ttc` is read-only. Install the rail and graphics/math
dependencies listed below and prepare MathJax with `make build` followed by
`./ttc --install-math` first; the target does not install dependencies or silently
omit unavailable suites.

Rail integration additionally needs `bubblewrap`, `tmux`, and permission to
create unprivileged user/PID/UTS namespaces. It checks mounts, denies, shared
loopback networking, TTC startup, detach/reattach and concurrent creation, plus
independent tmux bootstrap and containment of tmux client replacement commands.
It also checks SSH-agent socket forwarding and reattachment, service denies,
read-only Neovim/global Git config, and writable Zsh history. Unit/CLI tests verify TTC's
SSH_AUTH_SOCK removal and global opt-in. Offline `ssh -G` probes verify system
drop-in snapshot ownership, preserved symlinks, read-only files, deny masks and
refresh on recreation rather than reattachment. It uses private scratch fixtures
and no credentials or external network access.

The automated demo checks server-side request shapes, all tool types and their
effects, isolated child context, edits, and exact Markdown source. The TUI
receives interleaved argument deltas with both Responses completion snapshots;
the Kitty driver pauses one stream to capture its awaiting state and inspector.
The TUI also answers three question tabs using single choices, custom text,
automatic advancement and the Submit button, and verifies Ctrl+D/C exit behavior. Rendering
tests verify fixture content, narrow widths, SGR continuity, controls, and CJK
cells in a simulated terminal. The original PTY workflow also proves two
foreground shells overlap and file writes retain their order with undo/redo.
It also switches from Standard to a catalog-advertised Fast choice while that
batch is running, checks that only the next request changes tier, and verifies
the persisted switch and background completion presentations. Unit tests cover
multiple live shell updates, expanded live inspection, and interruption. Job-read
regressions check filtered/cursor-bounded results, completed-row clicks, paging,
literal fenced output and inspection after reload; Kitty captures the returned page.

For actual Kitty screenshots on headless Arch Linux:

```sh
sudo pacman -S --needed kitty xorg-server-xvfb xorg-xauth mesa noto-fonts noto-fonts-cjk nodejs npm librsvg
./ttc --install-math               # optional math setup; pinned npm dependencies
make kitty-test                    # builds and captures real Kitty under Xvfb
python3 tests/kitty_visual.py      # reuse the current ./ttc binary
python3 tests/kitty_visual.py --tmux # private tmux; Kitty identity cleared
python3 tests/kitty_visual.py --offline --tmux # palette/image/math and highlighted search
python3 tests/pty_math.py --tmux    # mock tmux metadata, no reply; real MathJax
python3 tests/pty_math.py --disable-color # explicit RGB-disable fallback
```

Kitty 0.49.1 was verified; the driver requires `kitten @ screenshot`. Xvfb runs
on an automatically selected private display and Mesa supplies software OpenGL.
Fonts cover the math/Unicode/CJK fixture. No desktop, real auth, or inference is
needed. The driver clears inherited NO_COLOR and TMUX in its new Kitty environment,
uses a private control socket, drives the three question tabs and Submit,
and saves PNG/text captures for expanded/collapsed live sidebar lists, narrow-pane
mouse scrolling, full workspace metadata, running states, fullscreen, actual formula images, image pan/zoom and confirmed
click delivery, questions, table headers, shell
commands/output with highlighted inspector details, and
the bordered colored system prompt viewer at both ends. It prints its artifact
directory under `/tmp/ttc/<uid>/kitty-visual-*`; logs and exception context
are saved there on failure. The copied demo project/history has its own private
`demo-*` directory recorded in `demo.json`.
The offline fixture also captures multi-term Ctrl-R matching and underlined matches.

Pressure tests cover 100,000 conversation messages, huge chunked messages and
more than 128 formulas without idle rerendering. Regression tests cover a pending
image archived by compaction, child reply/exit ownership, exact binary image
export, Markdown code containers, and cached MathJax rendering with hostile Node
configuration disabled. Streaming-message growth and shrink preserve later
reading anchors and cache indices.
Installer/worker unit tests use self-contained executable fixtures without
network access. Real math tests/benchmarks run only when a validated optional
MathJax cache exists; run `./ttc --install-math` first to enable them. Benchmark
warm typesetting plus PNG conversion with
`go test ./internal/assets -run '^$' -bench BenchmarkMathJax -benchtime=10x`.

The `kitty_visual.py --tmux` mode enables passthrough and RGB in a private tmux, clearing
`KITTY_WINDOW_ID` inside it. It exercises automatic graphics
probing through tmux. The short `--offline` fixture uses the scripted provider,
a shell and an image thumbnail plus the notebook, without a TCP listener.
Both screenshot modes need local display/control sockets. Restricted sandboxes
may prohibit these and the HTTP mock's loopback listener; saved exceptions and
logs identify the environment limitation. Unit provider tests use in-process
HTTP handlers; the separate mock-server tests exercise real transport.
`pty_math.py` needs the built binary and prepared MathJax, Node and librsvg;
it needs no Kitty, Xvfb, display or socket. Its `--tmux` option simulates DCS
passthrough without starting tmux, and checks graphics without `COLORTERM`.
The full demo also checks stable instructions, immutable input prefixes and
reported cached/reasoning counters. Unit regressions distinguish unavailable
counters from zero, preserve model provenance, and avoid double-counting native
replay in estimates.

Regression tests round-trip native tool arguments through SQLite, including
formatted JSON, HTML text and large integers. Schema tests reject incompatible
databases without deleting history or credentials, and check
external symlink target preservation. Fullscreen tests verify mouse disable and
restoration for every exit path, full-column rendering, and keyboard questions.

`python3 tests/pty_parallel.py` runs simultaneous instances against one data root
and workspace, checks independent manual loads, and verifies that restart leaves
unfinished source records unchanged. `pty_history.py` checks multi-term Ctrl-R
recall. Search benchmarks use bounded synthetic prompts by default; set
`TTC_PROMPT_BENCH_DATA` to a private JSON string array to use local prompts.

The offline `python3 tests/pty_input.py` regression also runs a reproducible
file/shell fixture, launches `/btw explain result.py` while its foreground shell
waits, checks the Markdown popup and diff inspector, and verifies strict-shell
exit and blank-tail previews. It uses no credentials or inference. All fixture
files, scripts, SQLite history and complete terminal/failure logs are saved in
the printed scratch directory. To inspect visually, `make demo` shows the real
tool cards, diff inspectors and Markdown; in a live session run `/btw QUESTION`
while a turn is working and dismiss its result with Esc.

When local socket creation is restricted, the socket-free integration still uses
an actual mock HTTP server and OpenAI adapter, covering all twenty tools:

```sh
go test -race ./internal/session -run TestHTTPMockOpenAIIntegrationTwentyToolTypes -v
python3 tests/pty_input.py       # offline editor, history, steering and promotion
python3 tests/pty_compaction.py  # offline automatic compaction
```

No subscription credentials or inference are used. The interactive demo needs
permission to bind a loopback TCP socket.
