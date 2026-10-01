# TTC - True Terminal Coding

TTC is an opinionated coding agent for terminal-native research-heavy coding
workflows on headless servers. If you agree with the following, then TTC may
be helpful to you:
* Agents should be a collaborative tool instead of owning the full project.
* Full terminal workflow is productive. Agents should live in tmux as a normal
  progress instead of having its own background session management or a lifespan
  longer than its TUI.
* Agents should not try to decide the "safety" or a tool call or impose
  permissions or setup half-working sandboxes. The user is responsible to setup
  a proper isolated environment.
* Agents should provide a minimum set of useful tools to fully exploit the
  capability of LLMs.
* Agents only need to run on Linux hosts.

Besides conventional tools like file edit, web, and shell, TTC also supports the
following:
* Background subagent/jobs and wakeup timers to manage long-running tasks.
* Builtin file-based-plan and tmux skills.
* Inline and block math rendering using MathJax with Kitty display.
* Image display and the ability to pass a user clicked coordinate as tool result
  to the LLM.
* Automatic main-context compaction at request/tool boundaries. It keeps the
  user instruction and last two assistant messages with complete tool results
  when cutting within a turn. Jobs, timers and queued input continue. Earlier
  file edits become the undo baseline. `/compact [focus]` remains available.

TTC reads `AGENTS.md` from root through cwd and refreshes it at request boundaries;
the model reads deeper files before scoped edits. Skills load on demand from
ancestor/cwd `.agents/skills/*/SKILL.md`, `~/.agents/skills` and bundled defaults.
The requested `.agents/skill` path is also supported. Nearer project definitions
win, then user and bundled definitions; plural paths win at the same level.
Instruction documents must be regular files of at most 1 MiB; symlinks are followed.

The [requirements](docs/requirement.md), [implementation design](docs/design.md),
[tool contracts](docs/tools.md), [compaction design](docs/compaction.md), and
[system prompt](docs/system_prompt.md) describe the complete target.
TTC's design draws from [Codex](https://github.com/openai/codex),
[pi](https://github.com/earendil-works/pi), and
[OpenCode](https://github.com/anomalyco/opencode).

## Installation

Build on Linux with Go 1.26.5 or later. Install `rg` for recursive file search
and GNU `diff` (`diffutils` on Arch) for mutation previews. A missing diff
executable leaves edits usable and reports an unavailable preview:

```sh
make build
./ttc --login
./ttc                         # start with the last model choice
```

To explicitly reuse subscription credentials, copy them into TTC's private
store once:

```sh
./ttc --import-codex-auth "$HOME/.codex/auth.json"
```

For math, install Node, npm and `rsvg-convert` (`nodejs npm librsvg` on Arch).
Initialization uses `npm ci` to install SHA-512-pinned MathJax 4.1.3 into
`$XDG_CACHE_HOME/ttc/mathjax` or `~/.cache/ttc/mathjax`; no archive is embedded.
Run `./ttc --install-math` to prepare it without login. Rendering reuses a warm
process; ordinary images need none of these dependencies. See [math setup](docs/mathjax.md).

## Testing

Validation uses self-contained unit tests and a **local mock OpenAI HTTP
server**, with synthetic credentials and prefix-matched Responses/SSE scripts:

```sh
make test
make check
make integration
python3 tests/pty_input.py         # offline editor/completion/export regression
python3 tests/pty_compaction.py    # offline automatic compaction and discovery
make demo                          # interactive TUI, local mock server
```

The PTY test exercises HTTP retry notices and inspection, catalog discovery,
streamed tool calls/results, system prompt inspection, file undo/redo, export,
background completion, session-switch cancellation, and history reload. It saves
transcripts and failures in private scratch. Normal tests never make live
inference requests. The explicitly gated `TestLiveSmoke` was verified with one
request reporting 34 input and 7 output tokens. The broader demo exercises all
19 tool types on a copied research fixture and renders its Markdown notebook. It
uses no subscription credentials or inference. The mock also handles web search
locally. Type `Run demo`, answer the question, and inspect rows with a click or
Tab. [Demo instructions](tests/README.md) describe keyboard controls, automated
modes, and saved artifacts. The interactive demo includes a 320×200 image with a
requested click in a graphics-capable Kitty session.

For automated visual inspection on headless Arch Linux, install the test-only
Kitty/Xvfb dependencies and capture the real TUI as PNG images:

```sh
sudo pacman -S --needed kitty xorg-server-xvfb xorg-xauth mesa noto-fonts noto-fonts-cjk nodejs npm librsvg
make kitty-test
python3 tests/kitty_visual.py --tmux   # Kitty through private tmux, identity cleared
python3 tests/kitty_visual.py --offline --tmux  # shorter fixture, no TCP mock
```
