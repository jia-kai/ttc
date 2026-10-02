# TTC v1 design

TTC is a Go application for Linux hosts. This design defines the complete
first-version target; README.md describes setup and current behavior. TTC uses
one process, one active main session, and SQLite for history and reversible
file edits. [Requirements](requirement.md), [tool contracts](tools.md),
[compaction](compaction.md), and the [system prompt](system_prompt.md) define
the corresponding user and model behavior.

## Boundaries

| Durable across launches                       | Active-runtime memory                                    |
| --------------------------------------------- | -------------------------------------------------------- |
| Conversations and history branches            | Model streams and retry waits                            |
| Tool arguments, results and Markdown records  | Shell/LSP jobs, children and output buffers              |
| Model selections and request usage            | Timers and pending notifications                         |
| File snapshots and undo cursors               | Queued prompts, steers, questions and armed image clicks |
| Attachments and compaction archives           | Child contexts and concurrency slots                     |

- History records execution; loading never restarts jobs or timers. There is no
  daemon, IPC service, process adoption or durable scheduler. Long-lived research
  work belongs in user-managed tmux; detaching keeps TTC running.
- Exit, `/new`, `/clear` and loading another main session cancel/join model work,
  children, shell/LSP process groups and timers. Inspecting a child stays inside
  the current runtime.
- Compaction changes conversation context inside that runtime, preserving live
  handles, jobs, timers and queued input. Completion handlers resolve its current
  session ID when committing. A routing read lock covers child commits and file
  tools; main handoff takes its write lock. Streams and blocking non-file tools
  never hold it. Coding instructions stay fixed; changed runtime state is appended.
- Graceful shutdown records interruptions and bounded output. Restart marks
  unfinished calls/turns interrupted without relaunching work or firing missed
  timers. An abrupt OS kill offers no process-cleanup guarantee. Managed commands
  must stay in their process group; daemonization is outside the shell contract.

## Go module structure

One Go module uses direct construction and small interfaces at their consumers:

| Package                    | Ownership                                           |
| -------------------------- | --------------------------------------------------- |
| `cmd/ttc`                  | CLI and dependency wiring                           |
| `cmd/embed-prompts`        | Build-time prompt validation and generation         |
| `internal/prompts`         | Immutable generated LLM text; no runtime files/YAML |
| `internal/session`         | Main/child turns, admission, input and timers       |
| `internal/provider`        | Models, streams, login and replay contracts         |
| `internal/provider/openai` | Subscription transport and device-code login        |
| `internal/tool`            | Registry, codecs, dispatch and tool implementations |
| `internal/workspace`       | Runtime-local serialized edits and restore    |
| `internal/history`         | SQLite branches, artifacts and exports              |
| `internal/context`         | Request projection and budgets                      |
| `internal/tui`             | Composer, windows, sidebar and login rendering      |
| `internal/jobs`            | Linux process groups and managed child tasks        |
| `internal/capture`         | Shared bounded stream rings and cursors             |
| `internal/scratch`         | Sticky root and verified private UID directory      |
| `internal/skills`          | Local/embedded discovery and precedence             |
| `internal/render`          | Markdown, tool presentation and plain output        |
| `internal/graphics`        | Kitty detection, protocol and passthrough           |
| `internal/lsp`             | Language servers, document sync and query results   |
| `internal/assets`          | Images, render cache and warm MathJax backend       |

- Author LLM text in [prompt/](../prompt/README.md): stable system/child/aside
  Markdown, compaction templates in `compaction.yaml`, tool guidance in
  `tools.yaml`, and naming text/limits in `naming.yaml`. Go retains parameter
  schemas, dynamic values and validation errors.
- `make prompts` validates UTF-8, required assets, YAML fields, tool coverage and
  naming limits, then atomically emits deterministic, git-ignored
  `internal/prompts/assets_generated.go`. `make build`, `make test` and `make check`
  generate first. Runtime consumers depend only on the leaf `internal/prompts`
  package; the executable needs no prompt files or YAML parser.
- Embed `default-skills` through its Go package. No plugin framework, service
  locator or reflection-based persistence.
- Read AGENTS.md root-to-cwd at request boundaries, supplying changes. The agent
  reads deeper instructions before scoped edits. Discover `*/SKILL.md` in user
  and ancestor `.agents/skill`/`.agents/skills`; exact-name precedence is nearest
  project, parent, user, bundled, with plural paths winning at the same level.
  Discovery reads metadata; the skill tool loads the selected body on demand.
- One session loop owns live state; bounded worker channels and contexts define
  lifetimes. Interrupting a turn leaves independent jobs alive. Questions suspend
  only their caller. Parent/child requests may overlap with no cycle limit.
- At most four child/asides run concurrently and four coding contexts are
  retained, including persistent idle ones. Children cannot spawn children.
  Each assignment explicitly chooses persistence, has fresh turn/job IDs and
  returns up to 8 KiB of its final answer plus an immutable message reference.
  Success retains context only when requested; otherwise close and join owned
  background work. Failure/cancellation always close. See [tools](tools.md#subagent)
  and [compaction](compaction.md) for assignment and actor-handoff contracts.

## Event ordering and main timeline

- One serialized writer/admission gate commits semantic events from all actors,
  with monotonic `event_seq`, actor, causal request/call and chronological main
  turn. `undo_owner_turn_id` identifies the latest admitted human turn, not a
  notification-only inference turn. Workers never choose their history position.
- SQLite AUTOINCREMENT allocates original event identity durably; copies retain
  `source_id`. `Entry.EventSeq()` returns that source or the original row ID.
  Physical copy IDs address projections. Restart, branching and retention never
  reset the allocator; gaps are harmless and timestamps never break ties.
- The lineage follows commit chronology. Actor input is an explicit projection:
  summary, retained source events, subsequent work. Copies/cursor changes create
  neither new historical actions nor repeated notifications. Queued prompts and
  steers remain transient until admission.
- Commit provider-ordered tool intents before dispatch and terminal results once
  in observed completion order. Canonical input reconstructs results in call
  order. Deltas/progress update live UI objects without allocating history events.
- Human-turn admission and complete local file apply/history commits share the
  workspace gate. Save input and its file checkpoint before releasing it. Edits
  use the current main inference turn and latest human undo owner at commit;
  preserve launch attribution separately. Notification wakes create no human
  checkpoint. Shell/external writes are outside this ordering.
- Acquire workspace admission/mutation before the history writer; never wait for
  a worker holding the workspace gate. No SQL transaction spans filesystem work,
  model/UI calls or worker joins.
- Request admission freezes a committed cutoff, job/timer context and eligible
  notifications in sequence order. Commit delivered IDs and versioned message
  counts, not another transcript. Unpublished updates cannot alter input; failed
  admission consumes nothing or advances no actor cursor.
- Append inspectable runtime context only when state, project instructions or
  transitions change; activation/compaction force it. Retry an admitted request
  with identical in-memory input. Canonical history/archives retain exact messages.
  Delivery means inclusion through a result or notice at admission, not successful
  inference or merely recording a result.
- Admit idle wakes ahead of queued human input through the same gate. Repeating
  timers coalesce to the latest immutable firing at/before the cutoff, with its
  cumulative count and sequence. Acknowledgment advances only through that firing;
  later arrivals stay pending and cannot mutate admitted payloads.
- Publish transitions and notices together. Child finish commits terminal state,
  idle/closed state and `child_turn_finished` before another assignment. Foreground
  results carry that event for acknowledgment; background queues one notice, with
  no extra child `job_exit` or redundant wake. Child handoff and `child_compacted`
  commit together; `/btw` events are UI-only.
- Main compaction retains committed tails and moves the current conversation;
  child compaction moves only its actor cursor, not main read-only/undo state.
  Neither loses pending events nor replays delivered ones. Closing rejects new
  admissions, joins workers, records terminal events and discards model delivery.

This defines committed ordering, not repeatable worker completion order.

## Parallel tool execution

- Read-only calls (`read`, `glob`, `grep`, `skill`, `web_fetch`, `web_search`,
  `job_list`, `job_read`, `wakeup_list`, `lsp_query`) run concurrently. Then file
  mutations and remaining controls run in model order. Shells/subagents overlap
  both phases.
- Ordering is best effort within one response, not isolation from shells or
  children. Dependent calls require a later response. Join foreground calls before
  the next request. Ordinary errors do not skip siblings; storage failure cancels
  and joins the batch, settling unstarted calls where storage permits.
- Interrupted streams execute no calls. History/UI preserve completion order;
  actor input preserves original call order.
- Each runtime's workspace queue serializes complete read/validate/write/record
  operations and undo/redo across its actors. SQLite immediate transactions
  serialize short history commits across instances. Never hold SQL while waiting
  on models, users, filesystem operations or worker joins. Instances share data
  and workspaces without lifetime locks; workspace conflicts are the user's responsibility.

## Provider and model abstraction

Providers own authentication, catalogs and wire formats; frontends render typed
login steps. Providers never import widgets or print to the terminal.

```go
type Provider interface {
    Models(ctx context.Context) ([]ModelSpec, error)
    Login(ctx context.Context, ui LoginUI) error
    Stream(ctx context.Context, req Request, emit func(StreamEvent) error) error
    EstimateReplay(message Message) int
}
type LoginUI interface {
    Present(ctx context.Context, step LoginStep) (LoginAnswer, error)
}
```

- `Stream` stops on cancellation or callback error. OpenAI uses device-code login:
  show URL/code/expiry, poll at prescribed intervals, handle slowdown/expiry and
  save credentials atomically as 0600. A cancelable file lock covers credential
  reread/refresh/save across instances, never model requests or device authorization. No server browser, callback listener or
  API-key fallback. Secrets/codes never enter history or logs. Refresh is
  serialized; exit cancels login. `--import-codex-auth` explicitly copies credentials
  without modifying their source or switching billing mode.
- `ModelSpec` supplies stable provider/model IDs, display name, variants,
  capabilities and catalog revision. Variants are validated reasoning/tier presets.
  Save turn-start and per-request selections. Main picker changes apply after the
  current tool batch, with an inspectable switch; children keep their model, with
  reasoning changes only on explicit idle follow-up. Startup resolves remembered
  choices against a fresh catalog; CLI overrides are optional. See [models](models.md).

| Token field                | Meaning                                      |
| -------------------------- | -------------------------------------------- |
| `context_limit`            | Effective input-plus-output capacity         |
| `max_output_tokens`        | Endpoint ceiling, or zero when unpublished   |
| `output_allowance`         | Coding output reserve; cap where supported   |
| `estimation_margin`        | Estimation/wire-overhead reserve             |
| `recent_tokens_target`     | Desired recent history after compaction      |
| `next_turn_input_reserve`  | Free capacity for new input after compaction |
| `summary_output_allowance` | Summary reserve; cap where supported         |

- Reject unknown variants/options, nonpositive limits/allowances, negative
  reserves/ceilings and budgets unable to fit fixed instructions plus required
  summary/output/headroom. Providers supply defaults; never invent model limits.
  Subscription transport publishes neither an output ceiling nor an output-cap
  request field, so its allowance is a context reserve, not a generation cap.
- For capacity `C`, output `O`, margin `M` and estimated input `I`, compact when
  `I + O + M >= C`. A handoff must satisfy:

```text
fixed_input + summary_and_archive_links + retained_history + O + M
    + next_turn_input_reserve < C
retained_history <= recent_tokens_target (except mandatory partial-turn tail)
```

- [Compaction](compaction.md) defines shared main/child retention, archives,
  single-pass summary, concurrent tails and failure policy. The frontend reloads
  without a popup, preserving input/interactions. No context-rejection retry,
  summary regeneration or context-aware tool truncation.
- Estimate images, schemas and native replay once. `ReplayState` identifies its
  provider, underlying model and codec; native items replace canonical assistant
  messages on the wire. Validate text/calls, phase, item IDs and encrypted reasoning.
  Estimate model-visible occupancy, excluding transport metadata; OpenAI uses a
  coarse decoded-size reasoning estimate. Estimates never change bytes or usage.
- Compare stored JSON arguments after marshaler normalization, preserving large
  numbers and allowing RawMessage whitespace/HTML escaping. Stream completion
  snapshots instead compare exact strings. `ContextFor` strips foreign state from
  request copies, preserving originals; Standard/Fast of one base model are
  compatible. Compaction drops replay. Unsupported codecs/representations fail.
- Main steering waits for a model boundary; children accept idle follow-ups only.
  Native steering and provider stream recovery are outside v1. Retry transient
  failures until interrupted only before text/tool announcements/native items are
  committed. Positive `MaxAttempts` bounds total attempts; naming uses one.
- Before each cancellable retry wait, emit a typed event and hidden inspectable
  request-linked notice for main, child or compaction. Callback failure stops retry.
  Backoff starts at one second, doubles with 25% jitter, and caps at 30 seconds.
  Numeric/date Retry-After overrides it with that cap; zero/past permits immediate
  retry. Keep constant-size bookkeeping. Partial output fails without replay;
  restart never retries unfinished requests. Persist final aggregate metadata and
  separate notices, not a durable retry queue.
- Endpoint `Usage` is separate from estimates. Cache reads/writes are disjoint
  input subsets; reasoning is an output subset. Preserve zero versus unavailable.
  Uncached input subtracts reads; ordinary input also subtracts writes.
- The sidebar keeps the latest successful parent response and frozen model,
  separate from cumulative totals for all actors/naming/compaction. Add each
  response once, including later cache reads. Missing usage appears as coverage;
  an optional total is unavailable if any reported response omitted it.
- Copy counters under the runtime mutex. Match reported input to its producing
  request ID; percentage/numerical context usage includes reserves, with estimated
  components labeled separately. Handoff refreshes occupancy immediately while
  prior reported usage stays visible. Compaction preserves totals; explicit
  activation clears them. Mixed-model totals imply neither cost nor subscription
  allowance; pricing requires each producing model/tier.

## Inspectable request messages

- Save exact coding, naming and compaction instructions in private lineage
  artifacts per request. The TUI has one coding placeholder per actor pointing
  to its latest snapshot; internal request prompts remain distinct.
- Click a placeholder to open the shared scrollable `tui.Window`; plain output
  uses `/inspect <entry-id>`. Internal inputs/replies remain inspectable outside
  coding context. Authentication tokens, device codes and login steps never enter
  history, logs or inspector artifacts.
- Exact JSONL preserves versioned native reasoning/replay items. Markdown omits
  opaque state and summarizes internal requests instead of duplicating their
  inputs. Continuations drop opaque replay from coding requests.

See [system_prompt.md](system_prompt.md) for canonical sources and runtime use.

## Tool codecs and shared Markdown

- [Tool codecs](../internal/tool/tool.go) validate calls and encode concrete
  historical records with name/version. Decode never executes or revives handles;
  unknown versions fail. `Execution` carries stable IDs, cancellation and narrow
  services. Commit intent before dispatch, then exact arguments/result/Markdown.
  Validation errors retain original input in a dispatcher record.
- Delivered results are immutable, including background launch snapshots.
  Completion appends another entry. Restart settles unresolved foreground calls
  once as interrupted, without rerunning them; historical IDs are not live handles.
- `render.Markdown` separates bounded summary, detail and optional export body.
  Summary normally uses one clipped highlighted row; mutations add up to six
  short diff lines. Inspectors show paths, snapshot diffs and labeled parameters.
  Glob shows its pattern. Exact JSON remains in records/sidecars; click IDs are
  metadata. Escape/fence untrusted values and filter terminal controls.
- Persist presentation revision so CLI, inspector, export and compaction use the
  same saved document rather than future renderer behavior. Export defaults to
  inspector detail; system prompt Markdown is empty, but JSONL/inspection is exact.
- `Execution.Update` publishes transient cards; shell/child waits sample bounded
  tails every 250 ms. Replace by call identity and refresh live inspection without
  accumulating history. Final/background completion cards are immutable/durable.
- Capture defaults to 64 MiB per call (32 MiB per stream), sharing a 1 GiB runtime
  pool. Pressure evicts the least recently written other ring. Preview at most
  ten lines/1 KiB combined; `job_read` pages streams, EOF byte/line cursors and grep.
  Persist at most 8 KiB per stream at completion/stop; earlier ring output is lost
  on exit. Large durable details/attachments use private managed files.
- `/export <path>` freezes a committed cut and writes dense Markdown, exact
  `.jsonl` and a sibling assets directory with relative links. Reject existing
  targets; user exports remain outside retention cleanup. See [tools](tools.md)
  for result caps, preview budgets and rendering details.

## Serialized edits and shared undo

- One main undo history includes all parent/child file-tool edits. Child records
  retain actor/call provenance but stay outside parent model input. Children cannot
  restore independent branches or selectively undo dependent interleaved edits.
- Each human turn saves history/file tips. Idle child writes extend that turn's
  suffix; after another input, later commits belong to the new suffix regardless
  of launch time. The [admission gate](#event-ordering-and-main-timeline) fixes races.
- `/undo` requires idle main work, stops/joins jobs/children, clears transient
  input/timers and reverses the latest human suffix, including trailing async
  edits. Failed/interrupted turns remain undoable after work stops. `/redo` reapplies
  saved bytes forward without rerunning tools or reviving handles. New input
  branches history. Branch restoration reverses to the common ancestor, then
  applies the selected suffix under the same idle/stop boundary.
- Ctrl-X G displays only admitted human inputs, in linear columns except at forks.
  Enter restores immediately before the input; Space inspects it. Arrows navigate
  nearest human ancestry over hidden entries. Archived/blocked inputs remain
  inspectable. `/branch ID` supports explicit balanced cuts; `/redo` then restores
  the previously selected branch.
- All file tools share apply/restore: capture bytes, mode, absence and created
  parent directories; moves are delete/create. Reject symlinks, special files and
  multiply linked regular files. Outside-workspace effects are marked non-undoable;
  shell writes are never checkpointed. Refuse restoration if affected paths differ
  from expected snapshots. Other runtimes, shells and editors are independent writers.
- Loading writable history snapshots its selected ancestry through the last
  balanced main tool exchange into an independent session, sharing immutable
  lineage assets. It preserves source records, hides old runtime messages from
  model input, and establishes an undo boundary at the imported tip. Archived
  predecessors remain read-only. No files, tools or live handles are restored.
- File writes use same-directory temporary files and atomic per-file renames.
  Multi-file edits/restores and their SQL commits are independent: partial edits
  record their applied subset when possible; failed restores keep the old cursor.
  Startup never settles unfinished requests, repairs files or replays operations.
  A crash may leave files ahead of saved history; manual loading uses saved context.

## Minimal SQLite schema

- Use local SQLite with foreign keys per connection, WAL, full sync and bounded
  busy timeout. The canonical [schema](../internal/history/schema.sql) owns table
  definitions. Entry/change/request IDs are monotonic; other IDs are opaque text.
  Times are UTC Unix milliseconds; typed codecs validate versioned JSON.
- A short startup lock serializes connection/WAL setup; it is released before
  session work. Empty databases initialize in one immediate transaction.
  Incompatible or nonempty unversioned schemas fail explicitly; no migrations,
  automatic resets or credential deletion. Select a new data directory for
  incompatible history.
- Private `model-choices.json` stores provider model/variant choices independently,
  capped at 64 KiB with invalid input rejected. A short file lock serializes
  read-modify-write saves across instances. Save before queueing selection.
  Preference failure rejects it; later SQLite switch failure keeps the active
  model and already accepted startup preference.
- Blank identity/name/selection stays in memory. Startup/new/clear insert nothing;
  first user admission atomically creates workspace/session/turn/message with the
  same runtime ID and empty undo baseline. Failure starts no inference or partial
  rows. Pickers use canonical workspace paths, including unregistered workspaces,
  and list only durable sessions.
- `entries.content_json` holds typed messages, origin labels or tool references,
  not duplicate results. Main input follows selected ancestry and `model_visible=1`;
  child records use the same tree with visibility false. Status is UI history,
  never a hidden instruction. Child execution remains memory-only.
- `file_changes.paths_json` is a versioned ordered list of paths, before/after
  absence/type/mode/hash/blob state and actual applied flags. Non-undoable effects
  include reasons/paths. No filesystem-operation journal or recovery state.
- Validate same-lineage references, acyclic parents, source IDs and file-tip
  agreement. Compaction copies source IDs/shared change references without reapply.
  Resolve retained checkpoints through source IDs in the continuation; if removed,
  use its summary baseline. Freeze predecessor and insert continuation in one
  transaction. Manual copies can have independent writable siblings in the same lineage;
  active request completions follow their own compaction successor chain.
- Use keyset pagination, indexed tool lookup and recursive ancestry/change CTEs.
  Export the latest saved tool record at/before its cut. Persist compact final
  request metadata/usage/response IDs and linked retry notices, not full duplicated
  prompts. Session lists/inspectors require no full transcript scan.
- Artifacts are private, lineage-owned and referenced by relative paths/hashes;
  deduplicate only within the lineage. No global blob or durable job/timer/inbox
  table. Cleanup expires whole lineages after thirty inactive days and atomically
  rechecks activity before deletion. Loaded sessions refresh activity hourly;
  copies protect their shared ancestry/assets. Asset deletion is best effort with
  no restart repair. Credentials and preferences remain outside retention.

## Validation

- Round-trip every tool codec; verify immutable results and stale-handle errors.
- Exercise mixed parent/child edits, idle child writes, branching and conflict-
  checked undo/redo across SQL/filesystem failures.
- Test compaction archive/commit boundaries with live jobs, timers and interruption;
  restart must never relaunch work. Check indexed queries on realistic histories.
- Run `make test` and `make check`; use self-contained mock providers and headless
  PTYs, then verify actual Kitty/tmux rendering separately. Subscription smoke
  tests require explicit authorization and a small inference budget.
- Exclude daemon mode, durable jobs/timers, adoption, native steering, cross-session
  undo and independent child undo.

## Design basis

The concrete boundaries above are TTC decisions, informed by:

- [Building effective agents](https://www.anthropic.com/engineering/building-effective-agents):
  simple composable tool and runtime boundaries.
- [Effective context engineering](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents):
  compaction and retrieval of exact supporting details.
- [OpenAI reasoning models](https://developers.openai.com/api/docs/guides/reasoning):
  context accounting and compatible reasoning continuity across tool calls.
- [OpenAI prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching):
  stable instruction/tool prefixes where the endpoint supports them.
- [OpenAI authentication](https://learn.chatgpt.com/docs/auth):
  device-code authorization from a headless terminal.

## Terminal rendering ownership

- The Responses adapter assembles deltas by output index/item ID after call
  announcement. Completion snapshots must exactly match accumulated arguments,
  name and call ID, without duplicate completion. Emit calls in output-index
  order only after `response.completed`; failed/truncated streams execute none.
  `call_start` is inspectable display metadata, not a tool intent. Cap SSE events/
  arguments at 8 MiB and retained response output at 32 MiB. See the
  [streaming contract](https://developers.openai.com/api/docs/guides/function-calling).
- The TUI owns a lazy transcript and indexed block heights. Scroll state keeps a
  block/source-byte anchor separately from follow-tail. Typeset only intersecting
  16 KiB source blocks; unseen heights are estimates. Resize preserves anchors;
  bounded layout caches serve drawing and hit-testing. Fences cross chunks;
  source mapping is exact for plain wrapping and approximate for decorations.
  Label estimated totals. Ctrl+D at bottom, Esc or submission resumes follow-tail.
- Assistant/tool snapshots use one identity-indexed replacement path. Completion
  replaces streamed text with Markdown; finalized objects reject late updates.
  Frontend alone owns transcript, screen and Kitty state. Assistant labels align
  left, bodies indent two cells; items own no refresh goroutines.
- Sidebar sections independently scroll/collapse copied request/job/timer
  metadata; never scan history or revive job IDs during drawing. A cancelable
  joined worker refreshes optional Git root/branch every five seconds with
  two-second deadlines and bounded output. Workspace header uses `Workspace.Root`.
- Fullscreen uses a frozen transcript snapshot with shared immutable sources and
  independent indices/anchors. Background work still updates the live view;
  periodic status/asset redraws and automatic dialogs pause. Keep keyboard scroll/
  resize, all columns and a bottom working indicator; hide sidebar, scrollbar and
  idle composer. Typing reveals an overlay. Disable/ignore mouse events for terminal
  copy. Every exit restores mouse reporting and the live source-reading anchor.

### Image and math assets

- Original `image_show` bytes are content-addressed private lineage artifacts.
  Derived thumbnails/formulas use a separate 256 MiB render cache with 30-day
  idle pruning. Keys include source, parameters, cell size and backend/lock hash.
- One cancelable frontend worker decodes visible assets and owns optional warm
  MathJax/librsvg work for conversation and Markdown inspectors. Queue/transmit
  only visible rows; cancel offscreen tasks and discard decoded thumbnails.
  An LRU holds up to 128 formula rasters; all retained pixels share 32 MiB.
- Independent initialization hands a pre-warmed Node process to that worker;
  exit cancels/joins both. Formula errors show warnings/labeled TeX. See
  [MathJax setup and limits](mathjax.md) for pinned packages, worker lifecycle,
  oversampling, filtering and placement budgets.
- Graphics reuse x/ansi's chunked PNG encoder and Unicode image/row/column
  placeholders. Detect before tcell owns input and serialize graphics/screen
  writes through one TTY wrapper. Positive detection adds RGB to a private
  terminfo copy even without `COLORTERM`; explicit color disable suppresses it.
  Virtual placements follow cell redraws; retain only viewport assets and delete
  only TTC-owned IDs on exit. tmux transfers use DCS passthrough.
- Direct detection queries the terminal. In tmux, bounded metadata reads client
  identity, effective pane passthrough and RGB; Kitty with on/all passthrough and
  RGB needs no reply. Missing identity, passthrough, RGB or failed commands get
  specific warnings. Mixed clients are outside the guarantee: tmux selects the
  current/recent client.
- Bordered previews map cell centers through fit/letterbox/pan/zoom to source
  pixels. Selecting a point and confirming OK are separate. Pending clicks belong
  to the requesting actor and reach its next boundary; child completion waits for
  its click. Compaction preserves interactions; lifecycle resets discard them.
- Reserve one actor interaction before source I/O without holding its mutex over
  decoding/filesystem work. Keep the slot until child consumption; exit clears
  request/reply. Concrete modal/load state uses runtime generations to reject
  stale lifecycle results while preserving compaction previews. Continuation replay
  pins archived live cards with original inspector IDs; child image commits use
  the routing gate.

### Composer and session windows

- Up/Down recalls original main user/steer messages across saved sessions, capped
  at 1000 entries/8 MiB. Exclude file snapshots, runtime notices and continuation
  copies. Current submissions, including commands, share the caps. Ctrl-R searches
  whitespace-separated case-insensitive substrings with AND semantics, in any
  order, newest first. Queries have at most 256 characters; render at most 128
  visible result rows with 512-byte previews. Fold full prompts once on opening;
  filter with deduplicated terms, longest first and early rejection. Highlight
  all occurrences in sanitized previews, merging overlaps and preserving Unicode
  positions. Enter recalls the exact prompt; Esc preserves draft.
- Slash completion is local. One joined/debounced directory worker scans at most
  10,000 entries/256 matches, not a recursive index. Accept only matching draft/
  cursor/generation results; snapshot selected attachments asynchronously. Paste
  and dialogs retain key priority.
- Ctrl-X E suspends terminal/graphics around a cancelable `$VISUAL`/`$EDITOR`
  using a 0600 scratch file. Drain runtime events while suspended. Failure preserves
  draft/cursor; valid UTF-8 edits up to 8 MiB replace without sending.
- `/sessions`/Ctrl-X L queries at most 100 workspace-filtered metadata rows, not
  transcripts. Group local activity dates: Today, Yesterday, six prior weekdays,
  then ISO dates. Headers cannot select; click selects, Enter uses idle `/load`.
- `/load` and `--session` preflight archives and copy writable history before
  activation. Failed preparation leaves the prior runtime usable; read-only
  history stays immutable. Every coding request uses current system instructions.
  Initial runtime context includes cwd, repository presence and branch; Git
  metadata is sampled outside drawing with a two-second bound. Project/live
  context refreshes per request. Explicit loads never restore source live state.
- Events carry generation to reject stale publications after session changes or
  same-session restore. Metering updates after responses and tool batches. Loads
  replay without an acknowledgment window; new/clear, undo/redo and export use
  brief conversation acknowledgments. Help/compaction/job/timer lists use windows;
  errors stay in conversation.

## Read-only side questions

- `/btw QUESTION` freezes the latest balanced main request while busy, or selected
  canonical history while idle. Model/context freeze at admission; stable coding
  instructions plus [prompt/btw.md](../prompt/btw.md) constrain the quick answer.
- The shared child loop advertises and dispatches only a filtered read-only
  registry: no shell, mutations, interactions, timers or child creation. Timer
  listing is runtime-wide; job visibility still follows child ownership.
- Each aside has its own actor/cache identity, hidden inspectable records,
  supervisor captures and a 64 KiB final-answer limit. It shares the four-task
  limit, survives compaction and is canceled/joined on switch/exit.
- Answers stay out of parent context and never enqueue wakes. Save Markdown and
  open the shared window once dialogs/editor/paste/fullscreen release focus.
  Queue at most 16 popups; older answers remain in history.
- Reject blank/read-only sessions and pending redo so an aside cannot discard
  redo. Source inspiration: [OpenCode BTW](https://github.com/dalekirkwood/opencode-btw/blob/main/commands/btw.md)
  and [pi-btw](https://github.com/dbachelder/pi-btw); no plugin code is imported.
