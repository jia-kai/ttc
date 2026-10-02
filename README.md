# TTC - True Terminal Coding

TTC is an opinionated coding agent for terminal-native, research-heavy coding
workflows on headless servers. If you agree with the following, then TTC may
be helpful to you:

* Agents should be collaborative tools instead of owning the full project.
* A fully terminal-based workflow is productive. Agents should live in tmux as
  normal processes instead of having their own background session management
  or a lifespan longer than their TUIs.
* Agents should not try to decide the "safety" of a tool call, impose
  permissions, or set up half-working sandboxes. The user is responsible for
  setting up a properly isolated environment.
* Agents should provide a small set of useful tools to make effective use of
  LLM capabilities.
* Agents only need to run on Linux hosts.

Besides conventional tools like file editing, web access, and shell execution,
TTC also supports the following:

* Background jobs, subagents, and wakeup timers.
* Built-in file-based-plan and tmux skills.
* Inline and block math using MathJax.
* Image display and confirmed point coordinates delivered to the LLM as a later
  runtime message.

TTC reads `AGENTS.md` and `.agents/skills`. TTC deliberately omits things like
MCP and plugins.

The [requirements](docs/requirement.md), [implementation design](docs/design.md),
[tool contracts](docs/tools.md), [compaction design](docs/compaction.md), and
[system prompt](docs/system_prompt.md) describe the intended behavior and
architecture. TTC's design draws from [Codex](https://github.com/openai/codex),
[pi](https://github.com/earendil-works/pi), and
[OpenCode](https://github.com/anomalyco/opencode).

## Installation

Build on Linux with Go 1.26.5 or later. Install `rg` for recursive file search
and GNU `diff` (`diffutils` on Arch) for edit previews. If `diff` is unavailable,
edits still work, but previews are unavailable.

TTC currently uses OpenAI's ChatGPT subscription authentication, not API keys.
Device-code login prints a URL and code that you can authorize from another
device; the server does not need a browser:

```sh
make build
./ttc --login
./ttc                         # start with the last model choice
```

Instead of logging in, you can explicitly copy existing Codex subscription
credentials into TTC's private store:

```sh
./ttc --import-codex-auth "$HOME/.codex/auth.json"
```

The core workflow works without graphics. Image display and rendered math
require a terminal with Kitty graphics support. For a headless server, use
Kitty locally and connect over SSH. When running through tmux, enable graphics
passthrough in your tmux configuration:

```tmux
set -g allow-passthrough on
```

Inside tmux, TTC checks its detected client identity (`client_termtype`), the
pane's effective passthrough setting, and the client's RGB feature. Kitty replies
to graphics queries may not reach the pane, so TTC does not require them there.
Direct connections use a graphics query. Detection failures show their cause.
In sessions with multiple attached clients, tmux selects a recently active client;
use Kitty for each client displaying TTC.

TTC reads terminal cell sizes in pixels. If SSH or tmux omits them, TTC warns
and uses an estimated 8×16 pixels per cell; graphics sizing may be inaccurate.

For math rendering, install Node, npm, and `rsvg-convert` (run `sudo pacman -S
nodejs npm librsvg` on Arch). Run `./ttc --install-math` to prepare MathJax
without login. Rendering reuses a persistent Node process; ordinary images need
none of these dependencies. See [math setup](docs/mathjax.md).

## Usage

Start TTC in your project directory, or select the workspace explicitly:

```sh
/path/to/ttc --workdir /path/to/project
```

Use `/help` for the keyboard and command guide. Exit with `/quit` or Ctrl+C.
Enter during work queues a new turn after the current turn finishes. Alt+Enter
steers the current turn after its LLM response and foreground tool batch finish;
running foreground shells delay delivery until they finish or move to background.
Click a message or tool row to inspect it, or use Alt+Up/Down to focus a row
and Tab on an empty composer. Use `/undo`
and `/redo` for file-tool edits; shell changes are not part of undo/redo.
Use `/rename NAME` to set a session title; automatic naming preserves it.

Ctrl-X G lists human inputs across history branches. Enter restores the state
before the selected input; Space inspects its message. Ctrl+D at the end of the
conversation resumes following new output.

Child activity uses colored `[Sub name]` badges. Models choose names of up to
four words; names over 26 characters display 23 characters plus `...`. Shell and
child inspectors show up to 8 KiB per stream; `job_read` can read earlier output.
Completed tool/job states display as “done”; model JSON keeps `completed`.

Background jobs, subagents, and timers belong to the active runtime. Compaction
preserves them; switching sessions or exiting cancels them. Use tmux for work
that needs to outlive TTC. Runtime snapshots are appended only when state changes;
newly activated and compacted contexts receive a fresh snapshot.

The sidebar separates current parent context from cumulative usage by all agents.
Input includes cached tokens; “Uncached input” excludes cache reads. Output
includes reasoning. Compaction inference adds to run totals, while its handoff
refreshes the current context estimate.

## Testing

Validation uses self-contained unit tests and a **local mock OpenAI HTTP
server** with synthetic credentials. Normal tests make no live inference
requests:

```sh
make test
make check                    # race tests and vet
make integration              # PTY workflows and automated demos; needs Python 3
make demo                     # interactive TUI, local mock server; needs Python 3
```

The demo exercises all 20 tool types on a copied research fixture and renders
its Markdown notebook. It requires no subscription credentials and makes no
live inference requests; the mock also handles web search locally. Type
`Run demo`, answer the question, and inspect rows with a click or Tab.

See [testing and demo instructions](tests/README.md) for coverage, individual
regressions, saved artifacts, and automated Kitty screenshots on headless
Linux.
