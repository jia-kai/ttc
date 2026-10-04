# TTC v1 design

TTC targets Linux with one Go process, one active main session and SQLite history/
reversible edits. This defines the complete v1 target; [README](../README.md)
describes setup and current behavior. Related contracts:
[requirements](requirement.md), [tools](tools.md), [compaction](compaction.md),
[system prompt](system_prompt.md).

## Boundaries

| Durable across launches                       | Active-runtime memory                                    |
| --------------------------------------------- | -------------------------------------------------------- |
| Conversations and history branches            | Model streams and retry waits                            |
| Tool arguments, results and Markdown records  | Shell/LSP jobs, children and output buffers              |
| Model selections and request usage            | Timers and pending notifications                         |
| File snapshots and undo cursors               | Queued prompts, steers, questions and armed image clicks |
| Attachments and compaction archives           | Child contexts and concurrency slots                     |

- History records execution, never restarts it. No agent daemon, IPC service,
  process adoption or durable scheduler. Long-lived work belongs in tmux;
  detaching keeps TTC running. `rail` has a separate ephemeral sandbox supervisor/
  tmux socket registry, not agent-job or conversation restoration.
- Exit, `/new`, `/clear` and main-session loads cancel/join model work, children,
  shell/LSP groups and timers. Child inspection stays in the current runtime.
- Compaction preserves live state. Completion handlers resolve the current session
  at commit. A routing read lock covers child commits/file tools; main handoff
  takes its write lock. Streams/blocking non-file tools hold neither. Coding
  instructions and changed runtime snapshots follow [system_prompt.md](system_prompt.md).
- Graceful shutdown records interruptions and bounded output; startup leaves saved
  execution records untouched. An abrupt OS kill guarantees no process cleanup.
  Managed commands must stay in their process group; daemonization is unsupported.

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
| `internal/workspace`       | Runtime-local serialized edits and restore          |
| `internal/history`         | SQLite branches, artifacts and exports              |
| `internal/context`         | Request projection and budgets                      |
| `internal/tui`             | Composer, windows, sidebar and login rendering      |
| `internal/jobs`            | Linux process groups and managed child tasks        |
| `internal/capture`         | Shared bounded stream rings and cursors             |
| `internal/scratch`         | Sticky root and verified private UID directory      |
| `internal/rail`            | Workdir-scoped Bubblewrap mounts and tmux lifecycle |
| `internal/skills`          | Local/embedded discovery and precedence             |
| `internal/render`          | Markdown, tool presentation and plain output        |
| `internal/graphics`        | Kitty detection, protocol and passthrough           |
| `internal/lsp`             | Language servers, document sync and query results   |
| `internal/assets`          | Images, render cache and warm MathJax backend       |
| `internal/blobcache`       | Shared disposable filesystem blob storage          |

- The leaf `internal/prompts` package contains generated assets; authoring,
  validation and build integration belong to [prompt/README.md](../prompt/README.md).
  Embed `default-skills` through its Go package; no plugin framework, service
  locator or reflection persistence.
- Read root-to-cwd AGENTS.md at request boundaries and supply changes; deeper
  instructions apply before scoped edits. Discover `*/SKILL.md` in user/ancestor
  `.agents/skill` and `.agents/skills`. Exact-name precedence: nearest project,
  parent, user, bundled; plural paths win at the same level. Discovery reads
  metadata; `skill` loads the selected body on demand.
- One session loop owns live state; bounded channels/contexts own worker lifetimes.
  Turn interruption leaves independent jobs alive. Questions suspend only main;
  children finish useful work, report gaps and stop without dialogs. Requests
  overlap with no cycle limit.
- The shared supervisor enforces [child capacity and persistence](tools.md#subagent)
  for coding assignments and read-only asides. Child context cuts follow
  [compaction](compaction.md#handoff) without moving main history or undo state.

## Rail layering

Rail's Linux implementation uses concrete intermediate representations, not a
backend/plugin framework:

1. **Configuration:** bounded file IO supplies parsed `filePolicy` layers. Pure
   resolution merges them into `Policy`: lexical paths, denies, protected config
   entrypoints and authorized service selections. It does not determine effective
   filesystem access; namespace-aware planning is authoritative.
2. **Specification:** `sandboxSpec` combines the central default-import catalog
   with explicit/service requests and captured host environment/UID. Requests
   retain source requirements, protection and origin, alongside the system SSH
   drop-in snapshot intent; this stage performs no filesystem inspection.
3. **Planning:** host probes and the evolving sandbox namespace resolve sources
   and destinations separately. Audits, access modes, collision checks and
   precedence produce `sandboxPlan`: ordered filesystem operations, namespace
   choices, environment changes, workdir/hostname and selected tmux config.
   Primary binds precede protective and SSH snapshot overlays; deny masks come
   last. Logical and discovered canonical dependencies order parents before
   children; newly revealed ancestors discard tentative placements and replay
   with the additional dependency.
   Host observations detect inconsistent reads during planning. Cancellation is
   checked between host syscalls, not within a blocking syscall.
4. **Emission:** pure Bubblewrap translation consumes the plan plus typed process
   settings (command, captured inherited environment, supervisor parent-death
   policy). It validates representation shape, never reads the host, reorders
   operations or makes access decisions. Planned set/unset operations override
   inherited variables. The resulting invocation owns its argument/environment
   slices and immutable file payloads. Generated files lower to `--ro-bind-data`
   with distinct inherited descriptors; tmux startup generation likewise consumes
   planned inputs.
5. **Execution:** the registry/supervisor owns locks, private runtime directories,
   process launch, readiness, cleanup and client transport. It supplies lifecycle
   settings but does not splice backend flags into an invocation. Immediately
   before launch, bounded revalidation rejects changes to observed host facts.
   Planned file bytes are materialized in sealed anonymous descriptors, whose
   parent handles close after start or on failure. Bubblewrap creates read-only,
   launcher-owned files and consumes the descriptors without exposing staging paths.

Specs/plans are transient owned values, not persisted state. SSH drop-in snapshots
use bounded descriptor-checked reads and overlay regular-file targets in the
effective namespace, preserving symlink paths. Denied files are not read; their
final masks are empty generated files with acceptable SSH ownership. Namespace
probes retain original sources for file shape/path reasoning, not generated
ownership or inode metadata. Revalidation covers observed identities, symlink
targets, relevant metadata and bounded SSH directory listings, not recursive
directory contents. Ordinary host binds can still change after validation;
selected snapshot bytes stay fixed, but directory membership and ancestor
replacement remain live. Configuration
and user-visible security limits remain in the [rail reference](../default-skills/ttc-config/SKILL.md#rail-configuration-reference).

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
- [Runtime snapshots](system_prompt.md#runtime-snapshots) supply immutable state at
  admission. Retry an admitted request with identical in-memory input. Canonical
  history/archives retain exact messages. Delivery means inclusion through a result
  or notice at admission, not successful inference or merely recording a result.
- Admit idle wakes ahead of queued human input through the same gate. Repeating
  timers coalesce to the latest immutable firing at/before the cutoff, with its
  cumulative count and sequence. Acknowledgment advances only through that firing;
  later arrivals stay pending and cannot mutate admitted payloads.
- Publish transitions and notices together. Child finish commits terminal state,
  idle/closed state and `child_turn_finished` before another assignment. Foreground
  results carry that event for acknowledgment; background queues one notice, with
  no extra child `job_exit` or redundant wake. Child handoff and `child_compacted`
  commit together; `/btw` events are UI-only.
- [Compaction handoff](compaction.md#handoff) uses this gate to retain committed
  tails and move actor context without losing pending events or replaying delivery.
  Closing rejects new admissions, joins workers, records terminal events and
  discards model delivery.

This defines committed ordering, not repeatable worker completion order.

## Parallel tool execution

- The dispatcher applies [tool batch ordering](tools.md#common-contracts):
  concurrent reads, ordered mutations/controls and overlapping shells/children.
  This orders one response, not external shells or child work.
- Join foreground calls before the next request. Ordinary errors do not skip
  siblings; storage failure cancels/joins the batch and settles unstarted calls
  where possible. Interrupted streams execute none. History/UI retain completion
  order; actor input retains call order.
- The workspace queue serializes full read/validate/write/record and undo/redo
  across runtime actors. SQLite immediate transactions serialize short commits
  across instances; never hold SQL while waiting on models, users, filesystem
  work or joins. No lifetime data/workspace locks; cross-instance workspace
  conflicts are the user's responsibility.

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
  [Models](models.md) owns catalog/startup, selection, budget metadata and metering;
  [compaction](compaction.md) owns admission budgets, retention and failure policy.
  The frontend reloads after handoff without a popup, preserving input/interactions.
- Estimate binary files, schemas and native replay once. `ReplayState` identifies its
  provider, underlying model and codec; native items replace canonical assistant
  messages on the wire. Validate text/calls, phase, item IDs and encrypted reasoning.
  Estimate model-visible occupancy, excluding transport metadata; OpenAI uses a
  coarse decoded-size reasoning estimate. Estimates never change bytes or usage.
- Compare stored JSON arguments after marshaler normalization, preserving large
  numbers and allowing RawMessage whitespace/HTML escaping. Stream completion
  snapshots instead compare exact strings. `ContextFor` strips foreign state from
  request copies, preserving originals under [model compatibility](models.md#switching-and-replay).
  Compaction drops replay. Unsupported codecs/representations fail.
- Endpoint `Usage` remains separate from input estimates. The runtime owns copied
  counters; [usage accounting](models.md#usage-accounting) defines their subsets,
  response identity, coverage and reset rules.

### Request retries

- Main steering waits for a model boundary; children accept idle follow-ups only.
  Native steering and provider stream recovery are outside v1. Retry transient
  transport/rate-limit/server failures until interrupted only before text/tool
  announcements/native items are committed. Positive `MaxAttempts` bounds total
  attempts; naming limits come from
  [prompt/naming.yaml](../prompt/naming.yaml).
- Before each cancellable retry wait, emit a typed event and hidden inspectable
  request-linked notice for main, child or compaction. Callback failure stops retry.
  Backoff starts at one second, doubles with 25% jitter, and caps at 30 seconds.
  Numeric/date Retry-After overrides it with that cap; zero/past permits immediate
  retry. Keep constant-size bookkeeping. Partial output fails without replay;
  restart never retries unfinished requests. Persist final aggregate metadata and
  separate notices, not a durable retry queue.

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
- Delivered results, including background launch snapshots, are immutable;
  completion appends another entry. Historical IDs are not live handles.
- `render.Markdown` separates bounded summary, detail and optional export body.
  Persist its revision/document so CLI, inspector, export and compaction never
  substitute a future renderer. Export defaults to detail; system-prompt Markdown
  is empty while JSONL/inspection remains exact. Keep exact JSON and click metadata
  separate; escape/fence untrusted values and filter terminal controls.
- `Execution.Update` replaces transient cards by call identity without history
  accumulation; final/completion cards are immutable/durable.
  [Tools](tools.md#saved-presentation-and-live-updates) defines update cadence and
  row, diff, inspector and output limits.
- Shared capture rings bound shell/child memory independently of durable records.
  [Shell](tools.md#shell), [job_read](tools.md#job_read) and
  [saved presentation](tools.md#saved-presentation-and-live-updates) own capture,
  preview, cursor and saved-output limits. Large durable details/attachments use
  private managed files.
- `/export <path>` freezes a committed cut and writes dense Markdown, exact
  `.jsonl` and sibling assets with relative links. Reject existing targets;
  user exports are outside retention cleanup.

## Serialized edits and shared undo

- Parent/child file tools share one main undo history. Preserve child actor/call
  provenance outside parent input; no independent child branch or selective undo
  of interleaved edits.
- Each human turn saves history/file tips. Idle child writes extend its suffix;
  later commits follow newly admitted input regardless of launch time. The
  [admission gate](#event-ordering-and-main-timeline) fixes races.
- `/undo` requires idle main, stops/joins jobs/children, clears transient input/
  timers and reverses the latest human suffix, including async edits and failed/
  interrupted turns. `/redo` restores saved bytes, never tools/handles. New input
  branches; branch restore reverses to the common ancestor then applies the chosen
  suffix under the same idle/stop boundary.
- Ctrl-X G shows admitted human inputs, linear except at forks. Enter restores
  before the input; Space inspects; arrows follow nearest human ancestry across
  hidden entries. Archived/blocked inputs stay inspectable. `/branch ID` accepts
  balanced cuts; `/redo` restores the previously selected branch.
- All file tools share apply/restore: capture bytes, mode, absence and created
  parent directories; moves are delete/create. Reject symlinks, special files and
  multiply linked regular files. Outside-workspace effects are marked non-undoable;
  shell writes are never checkpointed. Refuse restoration if affected paths differ
  from expected snapshots. Other runtimes, shells and editors are independent writers.
- Loading writable history snapshots its selected ancestry through the last
  balanced main tool exchange into an independent session, sharing immutable
  lineage assets. It preserves source records and historical retained-input
  markers, hides old live runtime messages from model input, and establishes an
  undo boundary at the imported tip. Archived
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
- Model preferences live outside SQLite; [catalog/startup](models.md#catalog-and-startup)
  defines file locking, limits and selection failure semantics.
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
  deduplicate only within the lineage. No global durable blob table or durable
  job/timer/inbox table. Enforce the [retention policy](requirement.md#data-retention)
  with an atomic activity recheck before lineage deletion. Copies protect shared
  ancestry/assets.

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

- Anthropic: [composable agent boundaries](https://www.anthropic.com/engineering/building-effective-agents)
  and [compaction/retrieval](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents).
- OpenAI: [reasoning/context continuity](https://developers.openai.com/api/docs/guides/reasoning),
  [stable prompt-cache prefixes](https://developers.openai.com/api/docs/guides/prompt-caching)
  and [headless device-code authentication](https://learn.chatgpt.com/docs/auth).

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
  resize, all columns and a copy-mode footer; hide sidebar, scrollbar and
  idle composer. Typing reveals an overlay. Disable/ignore mouse events for terminal
  copy. Every exit restores mouse reporting and the live source-reading anchor.

### Binary, image and math assets

- Providers announce binary formats and byte limits in model metadata; request
  admission freezes them for tool execution. `read()` owns local classification,
  bounded descriptor reads and format validation; adapters own native encoding.
- LLM `read()` binary files are references: absolute original path and SHA-256
  checksum, MIME type and byte size, but no payload in the database. Successful reads
  cache the exact validated original bytes under their checksum.
  Canonical tool messages retain references through history/load/compaction.
  The provider adapter uploads cached bytes without reopening the source. A miss
  verifies the source and repopulates the cache; unavailable originals become
  explicit outgoing text notices without modifying history or tool association.
  Invalid references, cancellation and cache errors still fail requests. Native
  image tool outputs use backend-default detail, without client preprocessing;
  document inputs use native file parts, without local extraction/conversion.
- Original `image_show` bytes are content-addressed private lineage artifacts.
  Disposable original uploads and derived PNGs share `$XDG_CACHE_HOME/ttc/assets`
  (default `~/.cache/ttc/assets`), a 4 GiB file-content cap including recipe
  references/staging, excluding filesystem overhead, and 32 MiB per blob.
  Cache operations enforce 30-day idle retention, not a daemon. Hits refresh idle
  time; TTL/least-recently-used pruning may evict active blobs.
  All bytes are SHA-256-addressed and deduplicated; render-recipe mappings refer
  to these same blobs rather than storing a second payload.
  Render keys include source, parameters, cell size and backend/lock hash.
  Atomic publication and short-lived cancelable filesystem locks coordinate
  writes/access/pruning across instances. No blob payloads enter the database;
  durable lineage snapshots and MathJax dependencies are outside this budget.
- One cancelable frontend worker decodes visible assets and owns optional warm
  MathJax/librsvg work for conversation and Markdown inspectors. Queue/transmit
  only visible rows; cancel offscreen tasks and discard decoded thumbnails.
  An LRU holds up to 128 formula rasters; all retained pixels share 32 MiB.
- [MathJax](mathjax.md) owns setup, engine lifecycle, formula limits and rasterization.
  Initialization hands its pre-warmed Node process to this worker; exit
  cancels/joins both.
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
  pixels under [image_show](tools.md#image_show)'s confirmation and actor-delivery
  contract. Child completion waits for its click.
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
  [Runtime snapshots](system_prompt.md#runtime-snapshots) define initial metadata
  and per-request refresh. Explicit loads never restore source live state.
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
  supervisor captures and a 64 KiB final-answer limit. It shares the
  [supervisor limit](tools.md#subagent), survives compaction and is
  canceled/joined on switch/exit.
- Answers stay out of parent context and never enqueue wakes. Save Markdown and
  open the shared window once dialogs/editor/paste/fullscreen release focus.
  Queue at most 16 popups; older answers remain in history.
- Reject blank/read-only sessions and pending redo so an aside cannot discard
  redo. Source inspiration: [OpenCode BTW](https://github.com/dalekirkwood/opencode-btw/blob/main/commands/btw.md)
  and [pi-btw](https://github.com/dbachelder/pi-btw); no plugin code is imported.
