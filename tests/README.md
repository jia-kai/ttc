# Local demonstration

Requirements: Linux, Go 1.26.5+, Python 3, and ripgrep (`rg`). No OpenAI login,
network service, subscription usage, or Python packages are needed. Math-enabled
TUI startup may download pinned npm packages if its cache is not ready. The mock
OpenAI server listens only on loopback and uses synthetic credentials. It checks
requests and sends a predefined Responses/SSE conversation.

For visual inspection, run this inside your terminal or a tmux pane:

```sh
make demo
```

Type `Run demo` and press Enter. The question dialog has three question tabs and
a final Submit tab. Prompts and options form one block, with controls below; the
review numbers questions to match their tabs. Select recommended **Yes** with
Enter to advance, choose **Other**, and enter `Looks good.`. Press Enter to
advance to the checks. Select **Data** with Enter to advance to **Submit**, and
press Enter again to send the complete round. Each question is single-choice;
Space selects without advancing. Tab/Shift+Tab preserves answers while switching
tabs; Left/Right moves the cursor while editing text and switches tabs
otherwise. Free-form answers support the main input's Bash-like editing keys and
Ctrl+X E editor. Only Enter on Submit sends answers. Esc leaves text entry, then
dismisses; `/questions` reopens the pending dialog. A normal message instead
settles dismissed calls with `dismissed:true` and redirects the agent; local
commands leave them pending.

The fixture exercises 20 selected tool types: file read/search, three edit
types, shell, a reusable child with an idle follow-up and explicit close, a
managed stdio LSP query, background job list/read/stop, local web fetch/search,
skill loading, a saved image and its dimensions, questions, and three timer
tools. The final notebook demonstrates headings, tables, emphasis, quotes,
nested and ordered lists, checkboxes, code, links, Unicode, and inline/block TeX
math. The reusable child returns deterministic multiline evidence exceeding 8
KiB, beginning `CHILD-PREVIEW 4.0` and ending `CHILD-FULL-TAIL`. Its assistant
responses and final answers returned to main occupy single clipped rows, not
expanded bodies or answer JSON. Click either row to inspect the complete answer;
End reveals the distinctive tail, and Esc closes the dialog. On a
returned-answer row, D switches from the full response to completion tool
details. Kitty displays images and formula images with Unicode placeholders;
unsupported TeX stays readable. Click the field thumbnail for pan/zoom and
coordinate selection. In a graphics-capable interactive demo, OK sends the
requested point to the mock server; Esc sends cancellation. Ctrl+X F toggles
fullscreen copy mode, hiding conversation scroll decorations and disabling mouse
capture; select/copy with the terminal and scroll by Ctrl+U/D. The display
freezes while runtime work continues; periodic redraws and automatic question
dialogs pause until exit. Esc or Ctrl+X F restores normal mouse behavior. Ctrl+X
S opens the sidebar overlay; click headers to expand lists and wheel-scroll each
independently.

Inspect any message/tool by clicking its row, or use Alt+Up/Down to focus a row
and Tab on an empty composer. The system-prompt placeholder opens the exact
prompt used by that actor. Each actor has one prompt placeholder showing its
latest snapshot; exact snapshots are retained for every request without adding
model calls. Tool briefings normally use one clipped row with short
arguments/status and labeled output excerpts; clicking opens full saved details
with readable parameters/results. File mutations add short diff previews, with
path headers and highlighted diffs in inspectors. Glob shows only its pattern.
Generated IDs use readable prefixes and a 16-character URL-safe random suffix.
Inside the shared display window, use Page Up/Down, Ctrl+U/D, Home/End, and Esc.
Up/Down in the composer recalls prompts and restores drafts. Ctrl+U/D scrolls
the conversation; Ctrl+D at the bottom follows new output and never exits.
Ctrl+C clears a nonempty prompt, or exits from any view if the prompt is empty.
Try `/model` or Ctrl+X, then M to browse model families and variants; Esc
cancels. Keep Demo Sol/low selected while running the fixed interactive script.
After completion you can switch models freely, open `/sessions` to select/reload
a session, inspect history, and quit with `/quit`.

Each invocation makes a fresh private project copied from
`tests/fixtures/research` under `/tmp/ttc/<uid>/demo-*`. It prints that
directory. The original fixture is never modified. Requests, transcript,
coverage, history, and failure context are retained for automated runs;
interactive runs retain requests, project, and history. Restart `make demo` to
reset the predefined conversation. The shared `/tmp/ttc` root is sticky 1777;
each numeric UID directory is owned by that user and private (0700). Test
drivers reject unsafe existing paths.

`pty_compaction.py` accepts `--binary /path/to/ttc` for scratch-built binaries.
It checks reconstructed cursor-addressed screen text and exact durable completed
turn counts, rather than assuming each displayed phrase appears contiguously in
the PTY byte stream. Its artifacts include raw `terminal.log`, rendered
`screen.txt`, process/request/turn diagnostics in `state.json`, and
`failure.txt` on failure. Failed requests and early process exits are reported
immediately.

`pty_await.py --binary /path/to/ttc` uses socket-free scripted inference in a
plain PTY. It verifies main timer/input wakeups, coding-child timer/shell
wakeups, no completed turn while awaiting, and cancellation of a persistent
child's owned timers before it becomes idle. FIFO and frontend-event markers
coordinate the fixture; scripts, terminal logs, SQLite history, table snapshots,
and failure tracebacks remain in its printed private scratch directory.

For reproducible headless validation:

```sh
make full-test                    # all automated suites, including graphics/math
make ci-test                      # all automated suites except real Kitty tests
make integration                  # original workflow + plain and TUI demos
python3 tests/demo.py              # automated plain PTY
python3 tests/demo.py --tui        # real TUI in a PTY; switches model via menu
python3 tests/pty_input.py          # offline editor, input scrolling, Shift+Enter/paste, export
python3 tests/pty_control_strings.py # offline control-string idle timeout and fragmented APC
python3 tests/pty_history.py        # offline prompt recall/search across restart and /new
python3 tests/pty_parallel.py       # shared data/workspace, independent loads, no recovery
python3 tests/pty_await.py          # offline main/child awaiting, input wakeup, owned timer cleanup
python3 tests/pty_compaction.py     # offline context handoff and ancestor discovery
python3 tests/pty_compaction_reload.py # offline CLI/TUI: boundary reload and automatic pending delivery
python3 tests/pty_partial_retry.py # mock SSE: partial recovery after one edit, warning, undo
python3 tests/pty_subagent.py       # mock HTTP: disposable child, low variant, direct answer
python3 tests/pty_subagent.py --failure # bounded retry exhaustion, partial answer, warning and transcripts
python3 tests/pty_subagent.py --offline # socket-free foreground disposal and UTF-8 answer limit
python3 -m unittest discover -s tests -p test_scratch.py # scratch permissions
make check                        # race tests and vet
make rail-integration             # real filesystem sandbox and tmux lifecycle
```

`make ci-test` first runs math protocol tests (including both explicit
color-disable settings), preparing MathJax before real-renderer unit/race tests.
It then runs Go tests/race/vet, PTY and rail regressions, and Python unit tests.
GitHub Actions runs this target without Kitty or Xvfb dependencies.
`make full-test` depends on `ci-test` and adds all direct/tmux and mock/offline
Kitty modes, including PNG captures. Suites run sequentially even with `-j`; the
first failure stops the run. Manual demos and benchmarks are excluded. Run it
outside rail if `./ttc` is read-only. Install the rail and graphics/math
dependencies listed below first. Math-enabled TUI startup installs the pinned
MathJax packages automatically; a cold cache needs npm and network access. The
target does not install host dependencies or silently omit unavailable suites.

Rail integration additionally needs `bubblewrap`, `tmux`, and permission to
create unprivileged user/PID/UTS namespaces. It checks mounts, denies, shared
loopback networking, TTC startup, detach/reattach and concurrent creation, plus
independent tmux bootstrap and containment of tmux client replacement commands.
It also checks SSH-agent socket forwarding and reattachment, service denies,
read-only Neovim/global Git config, and writable Zsh history. Unit/CLI tests
verify TTC's SSH_AUTH_SOCK removal and global opt-in. Offline `ssh -G` probes
verify system drop-in snapshot ownership, preserved symlinks, read-only files,
deny masks and refresh on recreation rather than reattachment. It uses private
scratch fixtures and no credentials or external network access.

The automated demo checks server-side request shapes, its selected tools and
their effects, isolated child context, edits, and exact Markdown source. The TUI
receives interleaved argument deltas with both Responses completion snapshots;
the Kitty driver pauses one stream to capture its awaiting state and inspector.
The TUI also answers three question tabs using single choices, custom text,
automatic advancement and the Submit button, and verifies Ctrl+D/C exit
behavior. The TUI demo reconstructs cursor-addressed frames and verifies
mouse/keyboard inspection of both compact rows through the full answer tail. It
checks exact saved child messages and the 8 KiB parent prefix, truncation flag
and full-message reference. Plain output also checks compact rows without hidden
body or answer JSON; Kitty requires both row types after narrow-pane reflow.
Final notebook checks wait for the durable main-turn completion and exact saved
answer before testing screen readiness. Rendering tests verify fixture content,
narrow widths, SGR continuity, controls, and CJK cells in a simulated terminal.
The original PTY workflow also proves two foreground shells overlap and file
writes retain their order with undo/redo. It also switches from Standard to a
catalog-advertised Fast choice while that batch is running, checks that only the
next request changes tier, and verifies the persisted switch and background
completion presentations. Unit tests cover multiple live shell updates, expanded
live inspection, and interruption. Job-read regressions check
filtered/cursor-bounded results, completed-row clicks, paging, literal fenced
output and inspection after reload; Kitty captures the returned page.

For actual Kitty screenshots on headless Arch Linux:

```sh
sudo pacman -S --needed kitty xorg-server-xvfb xorg-xauth mesa noto-fonts noto-fonts-cjk nodejs npm librsvg python-pillow
make kitty-test                    # builds and captures real Kitty under Xvfb
python3 tests/kitty_visual.py      # reuse the current ./ttc binary
python3 tests/kitty_visual.py --tmux # private tmux; Kitty identity cleared
python3 tests/kitty_visual.py --offline --tmux # palette/image/math and highlighted search
python3 tests/kitty_visual.py --question-only # offline native-cursor regression; also supports --tmux
python3 tests/pty_math.py --tmux    # mock tmux metadata, no reply; real MathJax
python3 tests/pty_math.py --disable-color # explicit RGB-disable fallback
```

Kitty 0.49.2 was verified; the driver requires `kitten @ screenshot`. Most PNGs
are diagnostic artifacts; tmux-window-return regressions use Pillow to compare
image pixels, including recovery after terminal image-cache loss. Xvfb runs on
an automatically selected private display and Mesa supplies software OpenGL.
Fonts cover the math/Unicode/CJK fixture. No desktop, real auth, or inference is
needed. The driver clears inherited NO_COLOR and TMUX in its new Kitty
environment, uses a private control socket, drives the three question tabs and
Submit, and saves PNG/text captures for compact child response/returned-answer
rows and both inspectors at their full tails, expanded/collapsed live sidebar
lists, narrow-pane mouse scrolling, full workspace metadata, running states,
fullscreen, actual formula images, image pan/zoom and confirmed click delivery,
question editing with a native cursor and no inserted display gap, table
headers, shell commands/output with highlighted inspector details, and the
bordered colored system prompt viewer at both ends. It prints its artifact
directory under `/tmp/ttc/<uid>/kitty-visual-*`; logs and exception context are
saved there on failure. The copied demo project/history has its own private
`demo-*` directory recorded in `demo.json`. The offline fixture also captures
multi-term Ctrl-R matching and underlined matches.

Pressure tests cover 100,000 conversation messages, huge chunked messages and
more than 128 formulas without idle rerendering. Regression tests cover a
pending image archived by compaction, child reply/exit ownership, exact binary
image export, Markdown code containers, and cached MathJax rendering with
hostile Node configuration disabled. Streaming-message growth and shrink
preserve later reading anchors and cache indices. Installer/worker unit tests
use self-contained executable fixtures without network access. Real math
tests/benchmarks run only when a validated optional MathJax cache exists; run
`make build` and `python3 tests/pty_math.py` first to prepare it through
automatic startup. Benchmark warm typesetting plus PNG conversion with
`go test ./internal/assets -run '^$' -bench BenchmarkMathJax -benchtime=10x`.

The `kitty_visual.py --tmux` mode enables passthrough and RGB in a private tmux,
clearing `KITTY_WINDOW_ID` inside it. It exercises automatic graphics probing
through tmux. The short `--offline` fixture uses the scripted provider, a shell
and an image thumbnail plus the notebook, without a TCP listener. Both
screenshot modes need local display/control sockets. Restricted sandboxes may
prohibit these and the HTTP mock's loopback listener; saved exceptions and logs
identify the environment limitation. Unit provider tests use in-process HTTP
handlers; the separate mock-server tests exercise real transport. `pty_math.py`
needs the built binary, Node and librsvg, plus npm/network for a cold MathJax
cache; it prepares MathJax automatically and needs no Kitty, Xvfb, display or
socket. Its `--tmux` option simulates DCS passthrough without starting tmux, and
checks graphics without `COLORTERM`. The full demo also checks stable
instructions, immutable input prefixes and reported cached/reasoning counters.
Unit regressions distinguish unavailable counters from zero, preserve model
provenance, and avoid double-counting native replay in estimates.

Regression tests round-trip native tool arguments through SQLite, including
formatted JSON, HTML text and large integers. Schema tests reject incompatible
databases without deleting history or credentials, and check external symlink
target preservation. Fullscreen tests verify mouse disable and restoration for
every exit path, full-column rendering, and keyboard questions.

`python3 tests/pty_parallel.py` runs simultaneous instances against one data
root and workspace, checks independent manual loads, and verifies that restart
leaves unfinished source records unchanged. `pty_history.py` checks multi-term
Ctrl-R recall. Search benchmarks use bounded synthetic prompts by default; set
`TTC_PROMPT_BENCH_DATA` to a private JSON string array to use local prompts.

The offline `python3 tests/pty_input.py` regression also runs a reproducible
file/shell fixture, launches `/btw explain result.py` while its foreground shell
waits, checks the Markdown popup and diff inspector, and verifies strict-shell
exit and blank-tail previews. It checks exact saved text after Ctrl+J/K (no
newline or deletion), CSI-u Shift+Enter, bracketed multiline paste and external
editing, without depending on composer row coordinates. A PTY injects encoded
keys; use `make demo` in the actual terminal client to check that Shift+Enter is
reported distinctly and to inspect six-row growth, soft wrapping and viewport
scrolling. It uses no credentials or inference. All fixture files, scripts,
SQLite history and complete terminal/failure logs are saved in the printed
scratch directory. To inspect visually, `make demo` shows the real tool cards,
diff inspectors and Markdown; in a live session run `/btw QUESTION` while a turn
is working and dismiss its result with Esc.

When local socket creation is restricted, the socket-free integration still uses
an actual mock HTTP server and OpenAI adapter, covering the twenty demo-selected
tools:

```sh
go test -race ./internal/session -run TestHTTPMockOpenAIIntegrationTwentyToolTypes -v
python3 tests/pty_input.py       # offline editor, history, steering and promotion
python3 tests/pty_compaction.py  # offline automatic compaction
```

No subscription credentials or inference are used. The interactive demo needs
permission to bind a loopback TCP socket.
