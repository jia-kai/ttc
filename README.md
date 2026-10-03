# TTC - True Terminal Coding

TTC is an opinionated coding agent for terminal-native, research-heavy coding
workflows on headless servers. If you agree with the following, then TTC may
be helpful to you:

* Agents should be collaborative tools instead of owning the full project.
* A minimalistic, fully terminal-based workflow is productive. Agents should
  live in tmux as normal processes instead of having their own background
  session management or a lifespan longer than their TUIs. No need for
  complications like a server-client agent architecture.
* Agents should not try to decide the "safety" of a tool call, impose
  permissions, or set up half-working sandboxes. The user is responsible for
  setting up a properly isolated environment.
* Agents should provide a small set of useful LLM-facing tools to make effective
  use of LLM capabilities.
* Agents only need to run on Linux hosts.

Besides conventional tools like file editing, web access, and shell execution,
TTC also supports the following:

* Background jobs, subagents, and wakeup timers.
* Built-in file-based-plan and tmux skills.
* Image display and confirmed point coordinates delivered to the LLM as a later
  runtime message.
* Inline and block math using MathJax.
* Transparency of all internal processes. Click a message or tool row to inspect
  raw messages and saved tool details.

TTC reads `AGENTS.md` and `.agents/skills`. TTC deliberately omits things like
MCP, plugins, different modes like plan mode.

The [requirements](docs/requirement.md), [implementation design](docs/design.md),
[tool contracts](docs/tools.md), [compaction design](docs/compaction.md), and
[system prompt](docs/system_prompt.md) describe the intended behavior and
architecture. TTC's design draws from [Codex](https://github.com/openai/codex),
[pi](https://github.com/earendil-works/pi), and
[OpenCode](https://github.com/anomalyco/opencode).

## Screenshots

A few examples of TTC's terminal-native workflow:

**Images in the conversation.** Generate an image and view it alongside the
commands and edits that produced it—here, a pelican riding a bicycle.

![TTC conversation showing a generated illustration of a pelican riding a bicycle](docs/screenshots/pelican.png)

**Interactive plots.** Open a plot, pan or zoom, and confirm a point to send its
pixel coordinates back to the agent.

![TTC image viewer displaying a plot with point selection and pan and zoom controls](docs/screenshots/plot.png)

**Rendered math.** Inline and block formulas make explanations easier to follow,
including converting a selected point from image pixels to plot coordinates.

![TTC rendering mathematical formulas and reporting the coordinates of a selected plot point](docs/screenshots/math.png)

**Inspectable tool calls.** Open a tool row to see its parameters, command,
status, and captured output.

![TTC tool inspector showing a shell command, its parameters, and its output](docs/screenshots/inspect.png)

## Installation

Build on Linux with Go 1.26.5 or later. `make` generates embedded
[prompt assets](prompt/README.md) before compiling. Install `rg` for recursive file search
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

Web search uses Exa's public, rate-limited endpoint by default. To use your own
key, create a private tool config (never included in LLM prompts):

```sh
mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}/ttc"
install -m 600 /dev/null "${XDG_CONFIG_HOME:-$HOME/.config}/ttc/web-search.json"
```

Edit that file to contain `{"api_key":"YOUR_EXA_KEY"}`. Optional `endpoint`
selects an HTTP(S) backend URL. `--web-search-config PATH` selects another file;
restart TTC after changes. Keep credential files private (use mode 0600). No key is
supplied through the `web_search` tool arguments.

Use Kitty as the terminal client for TTC's graphics features. When
running through tmux, enable graphics passthrough in your tmux configuration:

```tmux
set -g allow-passthrough on
```

For math rendering, install Node, npm, and `rsvg-convert` (run `sudo pacman -S
nodejs npm librsvg` on Arch). Run `./ttc --install-math` to prepare MathJax
without login. Rendering reuses a persistent Node process; ordinary images need
none of these dependencies. See [math setup](docs/mathjax.md).

## Usage

Start TTC in your project directory, or select the workspace explicitly:

```sh
/path/to/ttc --workdir /path/to/project
```

Use `/help` for the keyboard and command guide. Up/Down recall prompts across
sessions and restarts. Ctrl-R searches the most recent 1000 prompts (at most
8 MiB), newest matches first. Every whitespace-separated term must match a
case-insensitive substring, in any order; matches are highlighted. Enter fills
the input and Esc cancels.

Multiple TTC instances can share the default data directory and the same
workspace. Each starts an independent conversation; workspace conflicts are
your responsibility. `/load ID` or `--session ID` copies writable history into a
new session at its last complete tool exchange, with only new work undoable.
Archived predecessors open read-only. Loading never restores files or live jobs.
TTC does not repair interrupted work; files can be ahead of saved history after
a crash. Incompatible history schemas are rejected; use a new `--data-dir`.

The first model request includes cwd, whether Git inspection found a repository,
and its branch (`detached` when HEAD has no branch). Git is optional.
Foreground shell commands default to a 20-second timeout and cannot disable it.
Background commands have no default timeout; they can set an explicit deadline.

Background jobs, subagents, and timers belong to the active runtime. Compaction
preserves them; switching sessions or exiting cancels them. Use tmux for work
that needs to outlive TTC.

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
