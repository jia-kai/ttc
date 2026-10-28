# TTC v1 design

TTC targets Linux with one Go process, one active main session and SQLite
history/ reversible edits. This defines the complete v1 target;
[README](../README.md) describes setup and current behavior. Related contracts:
[requirements](requirement.md), [tools](tools.md), [compaction](compaction.md),
[system prompt](system_prompt.md).

## Boundaries

| Durable across launches                      | Active-runtime memory                                    |
| -------------------------------------------- | -------------------------------------------------------- |
| Conversations and history branches           | Model streams and retry waits                            |
| Tool arguments, results and Markdown records | Shell/LSP jobs, children and output buffers              |
| Model selections, request usage and events   | Live timers and notification queues                      |
| File snapshots and undo cursors              | Queued prompts, steers, questions and armed image clicks |
| Attachments and diagnostic archives          | Child contexts and concurrency slots                     |

- History records execution, never restarts it. No agent daemon, IPC service,
  process adoption or durable scheduler. Long-lived work belongs in tmux;
  detaching keeps TTC running. `rail` has a separate ephemeral sandbox
  supervisor/ tmux socket registry, not agent-job or conversation restoration.
  `watch-run` records externally owned work in durable files; runtime watchers
  only observe those files and never adopt or signal the external workload.
- Exit, `/new`, `/clear` and main-session loads cancel/join model work,
  children, shell/LSP groups and timers. Child inspection stays in the current
  runtime.
- Compaction preserves live state. Completion handlers resolve the current
  session at commit. A routing read lock covers child commits/file tools; main
  handoff takes its write lock. Streams/blocking non-file tools hold neither.
  Coding instructions and runtime-context baselines/updates follow
  [system_prompt.md](system_prompt.md).
- Each child assignment owns a managed-work scope. Completion closes
  registration, cancels all commands/watchers/timers, and joins them before
  publishing the final outcome. Persistent children retain conversation/model
  context only. Scope cleanup fences late events against later assignments;
  watcher cancellation never signals external workloads. Main-owned work lasts
  until explicitly stopped or the runtime closes.
- Graceful shutdown records interruptions and bounded output; startup leaves
  saved execution records untouched. An abrupt OS kill guarantees no process
  cleanup. Managed commands must stay in their process group; daemonization is
  unsupported.

## Message semantics

`llm.Message.Type` is the canonical, persisted message classification. A closed
policy maps each semantic type to its model role and presentation category;
neither role nor runtime provenance is stored as a competing flag. Actor, event
sequence, request identity and input source remain separate metadata.

| Semantic type           | Model role  | Presentation                            |
| ----------------------- | ----------- | --------------------------------------- |
| `user_input`            | `user`      | Authored input                          |
| `assistant`             | `assistant` | Assistant response                      |
| `tool_result`           | `tool`      | Tool result                             |
| `system_instruction`    | `system`    | Inspectable instructions                |
| `developer_instruction` | `developer` | Inspectable instructions                |
| `runtime_context`       | `developer` | Compact runtime context                 |
| `compaction_context`    | `developer` | Inspectable Markdown compaction context |
| `runtime_notification`  | `user`      | Runtime notice; attributed child result |
| `runtime_instruction`   | `developer` | Inspectable runtime instructions        |
| `child_guidance`        | `user`      | Main-agent assignment guidance          |

- Producers assign types before admission. Human-authored JSON never changes its
  semantic identity. Unknown types and incompatible tool/replay/input metadata
  are rejected at persistence and provider boundaries.
- Model projection adds `message_type` to notification and child-guidance JSON
  objects, preserving their event-specific `type` and payload. This makes
  provenance explicit without elevating payload authority. Provider roles are
  derived from the semantic type; provider-native tool calls, attachments and
  assistant replay state retain their specialized encoding.
- `session.Event.MessageType` carries canonical identity to direct rendering.
  Presentation flags are derived view state, not another message classifier.
  Provider-boundary inspection exposes notifications in their actual
  model-facing representation, independently of their direct-rendered summaries.
- Progress, redraws, tool-stream updates and other transient UI events are not
  conversation messages. They keep their own lifecycle rather than entering
  model context merely to share a renderer.
- Legacy role/runtime records are converted at the history schema migration
  boundary. Runtime callers do not infer message types from legacy flags or
  content. History migration never restores live jobs, timers or watchers.

## Go module structure

One Go module uses direct construction and small interfaces at their consumers:

| Package                      | Ownership                                            |
| ---------------------------- | ---------------------------------------------------- |
| `cmd/ttc`                    | CLI, application policy and runtime lifecycle        |
| `cmd/embed-prompts`          | Build-time prompt validation and generation          |
| `internal/prompts`           | Immutable generated LLM text; no runtime files/YAML  |
| `internal/session`           | Main/child turns, admission, input and timers        |
| `internal/llm`               | Neutral LLM values and offline scripting             |
| `internal/providers`         | Neutral modules, configuration and selection         |
| `internal/providers/builtin` | Explicit production-module registration              |
| `internal/providers/openai`  | OpenAI auth, protocol, codecs and module wiring      |
| `internal/catalog`           | Neutral discovery, budgets, cache and workers        |
| `internal/auth`              | Typed authorization presentation contract            |
| `internal/binaryinput`       | Binary validation, filesystem resolution and cache   |
| `internal/tool`              | Registry, codecs, dispatch and tool implementations  |
| `internal/workspace`         | Runtime-local serialized edits and restore           |
| `internal/history`           | SQLite branches, artifacts and exports               |
| `internal/history/migrate`   | Isolated legacy decoding and startup migrations      |
| `internal/messagestream`     | Request presentation items and readable documents    |
| `internal/context`           | Request projection and budgets                       |
| `internal/tui`               | Composer, windows, sidebar and login rendering       |
| `internal/jobs`              | Process groups, managed tasks and external observers |
| `internal/watchrun`          | External wrapper records, byte logs and observation  |
| `internal/capture`           | Shared bounded stream rings and cursors              |
| `internal/scratch`           | Sticky root and verified private UID directory       |
| `internal/rail`              | Workdir-scoped Bubblewrap mounts and tmux lifecycle  |
| `internal/skills`            | Local/embedded discovery and precedence              |
| `internal/render`            | Markdown, tool presentation and plain output         |
| `internal/graphics`          | Kitty detection, protocol and passthrough            |
| `internal/lsp`               | Language servers, document sync and query results    |
| `internal/assets`            | Images, render cache and warm MathJax backend        |
| `internal/blobcache`         | Shared disposable filesystem blob storage            |

- The leaf `internal/prompts` package contains generated assets; authoring,
  validation and build integration belong to
  [prompt/README.md](../prompt/README.md). Embed bundled skills through
  `prompt/skills`; no plugin framework, service locator or reflection
  persistence.
- Jobs retain optional construction-time inspection callbacks that return typed
  fields and bounded stream policies outside supervisor locks. One acquisition
  and rendering path serves live windows and saved completion Markdown.
  Execution snapshots and exact tool JSON stay separate from presentation;
  history never restores callbacks. LSP inspection cannot read protocol stdout.
- Read root-to-cwd AGENTS.md at request boundaries and supply changes; deeper
  instructions apply before scoped edits. Discover `*/SKILL.md` in user/ancestor
  `.agents/skill` and `.agents/skills`. Exact-name precedence: nearest project,
  parent, user, bundled; plural paths win at the same level. Discovery reads
  metadata; the actor's main/child/aside role filters advertisements and
  explicit loads, without falling back to another source. `skill` loads the
  selected body on demand and checks current restrictions as well as discovery
  restrictions. See the [skill contract](tools.md#skill) for metadata and
  refresh semantics.
- One session loop owns live state; bounded channels/contexts own worker
  lifetimes. Turn interruption leaves independent jobs alive. Questions suspend
  only main; children finish useful work, report gaps and stop without dialogs.
  Requests overlap with no cycle limit.
- The shared supervisor enforces
  [child capacity and persistence](tools.md#subagent) for coding assignments and
  read-only asides. Child context cuts follow
  [compaction](compaction.md#handoff) without moving main history or undo state.

## History and frontend boundaries

- `history.Store` owns SQLite, schema knowledge and persistence transactions.
  Callers use typed operations rather than SQL or database handles. Request
  admission, compaction and file-tip changes remain atomic storage operations.
- `RequestAdmission` carries one admission's selection, input deliveries and
  optional accounting checkpoint. `RequestCompletion` carries typed attempts;
  completion atomically saves audit evidence and an immutable usage observation.
  Operational usage reads follow selected completion/source identities, not
  mutable latest-request diagnostics or JSON status searches.
- Startup and `/load` share session preparation and activation. Preparation
  validates the target, recovers notices and builds accounting against the
  target tool schemas before stopping old work. Activation joins old workers and
  installs prepared state without target-storage operations. Live-work reset,
  conversation accounting and process-wide usage totals have separate lifetimes.
- Durable history values and canonical `llm.Message` values are independent of
  SQL. Typed history reads assemble replay and inspection data; pure projections
  format loaded data without database or filesystem access. Large supported TUI
  inspectors retain bounded SQL slices and streamed instruction snapshots rather
  than loading complete records. Replay and request-inspection ancestry
  projections keep selected IDs outermost and look up entries by primary key,
  avoiding full-table scans of unrelated saved history. Plain inspection uses
  complete records.
- The frontend controller owns mutable UI state on one event loop. Lifecycle,
  input, runtime events, replay, inspection and drawing have separate handlers;
  workers return results to the loop rather than mutate that state.
  Cancellation, worker joins and generation checks remain explicit. The TUI
  consumes a small history-reader interface and does not decode storage
  envelopes.
- Model inspection captures provider-formatted request bodies immediately before
  HTTP attempts and assembled native output items as they complete. Headers and
  credentials are excluded. Attempts retain distinct outcomes; inspection
  records never enter model context. Canonical history continues to own context,
  undo and continuation independently of captured provider data. Interrupted
  output without a completed native item is not synthesized from text/tool
  deltas; its failure outcome remains recorded. Raw and readable artifacts are
  each limited to 256 MiB; capture or persistence failures stop inference rather
  than silently losing inspection evidence.
- The presentation stream merges boundary-derived inspection rows with direct
  human/assistant rendering and tool execution updates. Stable row identities
  are separate from explicit actions: direct conversation rows do not expose
  payload inspection, while tool execution details retain live refresh. Boundary
  rows expose readable structured content and the captured JSON separately.
- Each attempt has one stable request row, updated as native response items and
  the outcome arrive. Immutable revision targets keep inspections branch-safe;
  live delivery and replay share the same projection. Full sent payloads remain
  exact captures, but default details show item-level deltas against the
  previous request for the same actor/purpose on selected ancestry.
  Conversation/cache-key changes do not prevent comparison. Legacy history
  remains loadable and continuable without fabricated provider captures.
- Providers expose ordered native inspection items and normalized hashes,
  excluding display names and input indexes. Prefix/suffix-trimmed, bounded
  Myers matching suppresses equal middle runs and preserves repeated
  occurrences. Inspections identify their base and addition/removal/unchanged
  counts; first requests, previous captures without item references and
  exhausted diff budgets explicitly reset to a full baseline. Missing/corrupt
  referenced artifacts fail capture. Only added/changed current-item JSON is
  displayed, not unchanged definitions, settings or context. Exact full requests
  remain in R.
- Providers parse native payloads into `llm.InspectionDocument` sections and
  typed values. History and the frontend never interpret provider-specific JSON.
  The generic renderer highlights fields and keeps simple key/value pairs and
  short scalar lists inline. Compact views shorten long text/containers with
  counted head/tail summaries; E expands ordinary text and containers.
  Opaque/deep provider values remain summarized and are available exactly in raw
  captures. Both readable modes are followed by full prettified JSON of the
  delta items in the same window, plus newly captured responses/outcome. R opens
  exact full raw data; N/P selects the request, response items or outcome.
  Styling is transient and respects no-color; captured bytes are unchanged.
  Oversized or invalid prettified JSON produces an explicit display notice,
  without discarding readable details or the exact raw capture.
- Continued scrolling at an inspection page's bottom/top loads one adjacent
  page, entering at its top/bottom respectively. Merely reaching a boundary does
  not change pages; drawing never loads them. Brackets remain explicit page
  navigation, while Home/End stay within the current page.
- Tests that assert physical persistence or inject database faults use the
  test-only history helper, not a production database escape hatch.

## Rail layering

Rail's Linux implementation uses concrete intermediate representations, not a
backend/plugin framework:

1. **Configuration:** bounded file IO supplies parsed `filePolicy` layers. Pure
   resolution merges them into `Policy`: lexical paths, denies, protected config
   entrypoints and authorized service selections. It does not determine
   effective filesystem access; namespace-aware planning is authoritative.
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
   with the additional dependency. Host observations detect inconsistent reads
   during planning. Cancellation is checked between host syscalls, not within a
   blocking syscall.
4. **Emission:** pure Bubblewrap translation consumes the plan plus typed
   process settings (command, captured inherited environment, supervisor
   parent-death policy). It validates representation shape, never reads the
   host, reorders operations or makes access decisions. Planned set/unset
   operations override inherited variables. The resulting invocation owns its
   argument/environment slices and immutable file payloads. Generated files
   lower to `--ro-bind-data` with distinct inherited descriptors; tmux startup
   generation likewise consumes planned inputs.
5. **Execution:** the registry/supervisor owns locks, private runtime
   directories, process launch, readiness, cleanup and client transport. It
   supplies lifecycle settings but does not splice backend flags into an
   invocation. Immediately before launch, bounded revalidation rejects changes
   to observed host facts. Planned file bytes are materialized in sealed
   anonymous descriptors, whose parent handles close after start or on failure.
   Bubblewrap creates read-only, launcher-owned files and consumes the
   descriptors without exposing staging paths.

Specs/plans are transient owned values, not persisted state. SSH drop-in
snapshots use bounded descriptor-checked reads and overlay regular-file targets
in the effective namespace, preserving symlink paths. Denied files are not read;
their final masks are empty generated files with acceptable SSH ownership.
Namespace probes retain original sources for file shape/path reasoning, not
generated ownership or inode metadata. Revalidation covers observed identities,
symlink targets, relevant metadata and bounded SSH directory listings, not
recursive directory contents. Ordinary host binds can still change after
validation; selected snapshot bytes stay fixed, but directory membership and
ancestor replacement remain live. Configuration and user-visible security limits
remain in the
[rail reference](../prompt/skills/ttc-config/SKILL.md#rail-configuration-reference).

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
  order. Deltas/progress update live UI objects without allocating history
  events.
- Explicit user interruption persists a model-visible runtime developer
  instruction after tool settlement and before subsequent input or wakeup
  admission. Provider failures and frontend/runtime cleanup do not create this
  notice. Failed notice persistence blocks later inference through the existing
  admission-error guard. The notice neither starts inference nor changes the
  auto-wake pause policy. Normal compaction may summarize it; loads hide it like
  other runtime instructions.
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
- [Runtime context](system_prompt.md#runtime-snapshots) supplies an initial
  baseline and developer-role field updates at admission, not repeated activity
  inventories. Retry an admitted request with identical in-memory input.
  Canonical history/archives retain exact messages. Delivery means inclusion
  through a result or notice at admission, not successful inference or merely
  recording a result.
- Admit idle wakes ahead of queued human input through the same gate. Repeating
  timers coalesce to the latest immutable firing at/before the cutoff, with its
  cumulative count and sequence. Acknowledgment advances only through that
  firing; later arrivals stay pending and cannot mutate admitted payloads.
- Publish transitions and notices together. Child finish commits terminal state,
  idle/closed state and `child_turn_finished` before another assignment.
  Foreground results carry that event for acknowledgment; background queues one
  notice, with no extra child `job_exit` or redundant wake. Child handoff and
  `child_compacted` commit together; `/btw` events are UI-only.
- [Compaction handoff](compaction.md#handoff) uses this gate to retain committed
  tails and move actor context without losing pending events or replaying
  delivery. Closing rejects new admissions, joins workers, records terminal
  events and discards model delivery.

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
  work or joins. The shared process-lifetime data lock excludes schema upgrades,
  not concurrent session work; there is no lifetime workspace lock.
  Cross-instance workspace conflicts are the user's responsibility.

## Provider and model abstraction

The CLI uses Cobra root and `md`, `rail` and `watch-run` commands, with
generated command-specific help and parser errors. Root model/variant selection
is an interactive UI concern backed by remembered choices, not launch flags.

`llm` holds shared, effect-free values and deterministic offline scripting, not
production provider registration or services. Sessions consume only
`session.Inference`:

```go
type Inference interface {
    Stream(context.Context, llm.Request, func(llm.StreamEvent) error) error
    EstimateReplay(llm.Message) int
    TokenizerID(llm.ModelSpec) string
}
```

`TokenizerID` supplies the provider-owned calibration identity; sharing an
identity is TTC policy, not a claim of a provider tokenizer guarantee. Native
replay compatibility remains separate.

`internal/providers` defines neutral module contracts and configuration helpers.
Concrete subpackages own each provider's implementation and wiring; shared LLM
values remain independent of them. A `Module` has an `ID` and
`Configure(*pflag.FlagSet) (Factory, error)`; configuration registers
module-owned flags with independent state. `Configure` stages all module flags
and rejects invalid modules or collisions without partially changing the
application's flag set. After parsing, `Select` returns the requested configured
factory; empty or unknown IDs fail. Explicit flags owned by a nonselected module
also fail, even when set to their default values.

The factory receives a context and `Environment` (`DataDir`, optional HTTP
`Client`, and `ResolveBinary`) and returns `Components`: required `Inference`,
optional `Catalog` (`Bind` and `CachePath`), optional
`Authorize(context.Context, auth.UI) error`, and a non-secret startup `Notice`.
Construction starts no workers or resources requiring closure. The CLI owns
catalog policy, deadlines, manager workers and shutdown. Normal interactive
startup requires a catalog; fixed models can use a static `catalog.Source`.
Login orchestration invalidates the catalog, calls authorization, then refreshes
choices; modules do not own that lifecycle or presentation.

`builtin.Modules` returns a fresh explicit production-module list. Registration
is separate so neutral `providers` never imports its concrete subpackages. The
first entry is the CLI's `--provider` default; currently the sole entry is
OpenAI. No `init` registration, runtime plugins or provider JSON configuration
is used: flags are the module configuration mechanism. `--offline-script` is
separate deterministic test mode, not a registered production provider. It
bypasses provider construction and rejects `--login`, explicit `--provider` and
provider-specific flags rather than silently ignoring them.

- Transports own context-bound protocol HTTP, wire/replay codecs and in-request
  retries, not files, caches, background workers, login UI or conversation
  state. OpenAI accepts injected credential and binary-resolution functions;
  `openai.AccessTokens` contains only access token/account identity. ID and
  refresh tokens stay with the auth owner, never the inference dependency.
- `catalog.Source.Models` returns remote `ModelInfo` capacities/capabilities.
  The provider-neutral `catalog.Manager` applies TTC `Policy` to create budgeted
  `ModelSpec` snapshots and owns scoped cache storage and discovery lifetimes;
  [models](models.md#catalog-and-startup) defines cold/warm startup and refresh.
- `openai.Authenticator` owns credentials, refresh and login storage; frontends
  render `auth.UI` steps. OpenAI uses device-code login: show URL/code/expiry,
  poll at prescribed intervals, handle slowdown/expiry and save credentials
  atomically as 0600. A cancelable file lock covers credential
  reread/refresh/save across instances, never model requests or device
  authorization. No server browser, callback listener or API-key fallback.
  Secrets/codes never enter history or logs. Refresh is serialized; exit cancels
  login. `--import-codex-auth` explicitly copies credentials without modifying
  their source or switching billing mode.
- `ModelInfo` supplies remote IDs, display name, variants, capabilities,
  capacities and catalog revision; `ModelSpec` adds application budget reserves.
  A `Selection` freezes provider ID, model and variant. Variants are validated
  reasoning/tier presets. [Models](models.md) owns catalog/startup, selection,
  budget metadata and metering; [compaction](compaction.md) owns admission
  budgets, retention and failure policy. The frontend reloads after handoff
  without a popup, preserving input/interactions.
- Estimate binary files, schemas and native replay once. `ReplayState`
  identifies its provider, underlying model and codec; native items replace
  canonical assistant messages on the wire. Validate text/calls, phase, item IDs
  and encrypted reasoning. Estimate model-visible occupancy, excluding transport
  metadata; OpenAI uses a coarse decoded-size reasoning estimate. Estimates
  never change bytes or usage.
- Compare stored JSON arguments after marshaler normalization, preserving large
  numbers and allowing RawMessage whitespace/HTML escaping. The finalized
  argument event and completed output item compare as exact strings; streamed
  fragments do not. `ContextFor` strips foreign state from request copies,
  preserving originals under
  [model compatibility](models.md#switching-and-replay). Compaction preserves
  compatible replay for summary inference, then drops it from replacement coding
  input. Unsupported codecs/representations fail.
- Endpoint `Usage` remains separate from input estimates. The runtime owns
  copied counters; [usage accounting](models.md#usage-accounting) defines their
  subsets, response identity, coverage and reset rules.

### Adding a provider

Add `internal/providers/acme` with a `Module` constructor returning a
`providers.Module`. Keep its flag configuration, transport, credential owner (if
needed) and catalog binding in that subpackage, with separate types/files for
their responsibilities. Add one explicit entry in
`internal/providers/builtin/builtin.go`; keep OpenAI first to preserve the
default. Implement `session.Inference` and supply `catalog.Source` for discovery
(or a static source for fixed models). Return the binding and optional cache
path (empty disables disk caching), not a running `catalog.Manager`. No CLI,
catalog, session or TUI changes are needed to add a provider using these
contracts. For mutable authenticated identities, supply `Binding.Guard` to
validate the captured identity and hold credential synchronization across cache
commit and publication; scope fields alone do not prevent account-change races.
See `internal/providers/openai/module.go`. For native binaries, inject
`binaryinput.Resolve` or another resolver returning verified original
`BinaryPayload` bytes; the transport codec only encodes them.

Emit boundary captures through `Request.Observe`, preserving original payload
bytes. Implement provider-owned parsing into `llm.InspectionDocument` sections
and typed values; history and the frontend consume that neutral representation.

Canonical `llm.Message` values intentionally remain shared with history:
source/request identity, runtime/input provenance, authored display text and
attachment references support admission, inspection and compaction. They are not
wire fields; codecs project content, calls and compatible native replay state.
Likewise, request attempt limits and typed retry/partial-error values coordinate
transport retries with session-owned cross-request recovery, not durable
workers.

### Request retries

- Each model HTTP attempt has a 90-second model-response inactivity watchdog
  starting before the request is sent, covering headers and the first model
  event. Only recognized model-response events refresh it, including reasoning,
  text, tool-call progress and relevant lifecycle events; raw bytes, partial SSE
  frames, comments, pings and unknown events do not. Malformed events do not
  refresh the watchdog; parser validation errors terminate the attempt. This is
  not a total-duration limit; a healthy but silent generation can time out.
  Shorter caller deadlines still win. Expiry is a retryable upstream timeout,
  not user cancellation; committed output follows partial-response recovery
  below.
- Cancellation is scoped to one request attempt, never shared mutable provider
  state. Abandonment immediately cancels the local request and closes its body,
  then invokes best-effort remote cancellation with a fresh bounded context
  before retry or return. Normal completion releases local resources without
  remote cancellation. Cleanup failure must not replace the original request
  error. Codex subscription remote cancellation is a no-op: no supported
  endpoint is verified, and requests remain foreground streams, not background
  responses. Local abort does not guarantee upstream inference or billing stops.
- Catalog and authorization HTTP exchanges retain the raw-response inactivity
  watchdog: first response bytes, informational/final headers and nonempty body
  reads refresh a 90-second interval. Each exchange owns its timer and
  cancelable context, released on EOF, body close, failure or caller
  cancellation. These operations retain their existing retry policies.
- Main steering waits for a model boundary; children accept idle follow-ups
  only. Native steering and resuming the same partial stream are outside v1.
  Providers retry upstream HTTP, transport, stream and protocol failures
  regardless of code. Parser-private native items, including reasoning, do not
  block retry and are discarded on failure. Zero `MaxAttempts` selects nine
  total attempts; positive values bound the whole retry/recovery chain;
  `PriorAttempts` carries failed attempts across continuation requests and
  resets after a successful response. Naming and compaction use the same
  default; no-tools replies are buffered until completion and retried
  atomically.
- After partial output, only a provider-issued `PartialError` authorizes a new
  main/child/aside coding request. Persist canonical partial output without
  native replay state and settle emitted calls as not executed. Retain earlier
  tool outcomes, append a runtime developer warning from
  [prompt/recovery.md](../prompt/recovery.md), then continue through normal
  admission. Keep the exact warning pending across compaction. Do not replay the
  failed request or automatically repeat side effects. Callback and persistence
  failures, local validation, TLS certificate verification and cancellation
  stop. Finalized tool arguments, rather than streamed fragments, determine
  executable calls. An invalid finalized argument object or disagreement with
  the completed output item uses this recovery path under the request's retry
  policy; unfinished calls never execute.
- Before each cancellable retry wait, emit a typed event and hidden inspectable
  request-linked notice for main, child or compaction. Callback failure stops
  retry. Backoff starts at one second, doubles with 25% jitter, and caps at 150
  seconds. Numeric/date Retry-After overrides it with that cap; zero/past
  permits immediate retry. Keep constant-size retry bookkeeping; history retains
  partial output and recovery instructions. Restart never retries unfinished
  requests. Persist final aggregate metadata and separate notices, not a durable
  retry queue. Coding request metadata and hidden turn-end records include
  failure text for inspection. Each upstream failure retains bounded,
  control-safe allowed error fields and request IDs; unknown shapes expose field
  names/types, not arbitrary scalar values or bodies.

## Inspectable request messages

- Save exact coding, naming and compaction instructions in private lineage
  artifacts per request for canonical audit and accounting provenance.
- Compaction keeps the actor's coding system instructions and saves its frozen
  structured prefix plus final summary instruction as internal request messages
  in one transaction, outside live coding input and without acknowledging
  events. Canonical main-summary input/reply records remain in the predecessor;
  provider-boundary inspection rows and their artifact references survive in the
  continuation. Coding input acquisition skips hidden entry bodies.
- Interactive inspection uses the boundary stream described above. Plain output
  uses `/inspect <entry-id>` for readable saved records. Authentication tokens,
  device codes and login steps never enter history, logs or inspector artifacts.
- Exact JSONL preserves versioned native reasoning/replay items. Markdown omits
  opaque state and summarizes internal requests instead of duplicating their
  inputs. Continuations drop opaque replay from coding requests.

See [system_prompt.md](system_prompt.md) for canonical sources and runtime use.

## Tool codecs and shared Markdown

- [Tool codecs](../internal/tool/tool.go) validate calls and encode concrete
  historical records with name/version. Decode never executes or revives
  handles; unknown versions fail. `Execution` carries stable IDs, cancellation
  and narrow services. Commit intent before dispatch, then exact
  arguments/result/Markdown. Validation errors retain original input in a
  dispatcher record.
- Delivered results, including background launch snapshots, are immutable;
  completion appends another entry. Historical IDs are not live handles.
- `render.Markdown` separates bounded summary, detail and optional export body.
  Persist its revision/document so CLI, inspector, export and compaction never
  substitute a future renderer. Export defaults to detail; system-prompt
  Markdown is empty while JSONL/inspection remains exact. Keep exact JSON and
  click metadata separate; escape/fence untrusted values and filter terminal
  controls.
- `Execution.Update` replaces transient cards by call identity without history
  accumulation; final/completion cards are immutable/durable.
  [Tools](tools.md#saved-presentation-and-live-updates) defines update cadence
  and row, diff, inspector and output limits.
- Shared capture rings bound shell/child memory independently of durable
  records. [Shell](tools.md#shell), [job_read](tools.md#job_read) and
  [saved presentation](tools.md#saved-presentation-and-live-updates) own
  capture, preview, cursor and saved-output limits. Large durable
  details/attachments use private managed files.
- `/export <path>` freezes a committed cut and writes dense Markdown, exact
  `.jsonl` and sibling assets with relative links. Reject existing targets; user
  exports are outside retention cleanup.
- [`conversation_export`](tools.md#conversation_export) instead writes one
  actor-selected Markdown or JSONL file in scratch shared by the UID, using
  atomic exclusive creation with mode 0600. Contents stream into the created
  file and may be partially visible before success; there is no final-rename
  publication guarantee. Serialization streams, but selected entry payloads
  remain buffered. SQLite selection follows compaction and load-copy ancestry
  and deduplicates source identities, never traversing sibling branches or
  unrelated actors. Main selects main history, children their own history across
  assignments, and asides their recorded frozen main prefix plus their own
  history. Full scope ends at the committed call cut; before-compaction scope
  ends at the latest successful actor cut. Scratch is ephemeral; exporting does
  not pin attachment cache bytes.
- Compaction creates no eager main/child/aside transcript files. Shared
  preparation selects and budgets actor history and generates the summary; main
  continuation and child/aside handoffs own their distinct commits. Child
  failure/cancellation still writes immutable diagnostic transcripts. Existing
  archive files, saved summaries and immutable evidence are preserved by
  migration, not rewritten or required for loading.

## Serialized edits and shared undo

- Parent/child file tools share one main undo history. Preserve child actor/call
  provenance outside parent input; no independent child branch or selective undo
  of interleaved edits.
- Each human turn saves history/file tips. Idle child writes extend its suffix;
  later commits follow newly admitted input regardless of launch time. The
  [admission gate](#event-ordering-and-main-timeline) fixes races.
- `/undo` requires idle main, stops/joins jobs/children, clears transient input/
  timers and reverses the latest human suffix, including async edits and failed/
  interrupted turns. `/redo` restores saved bytes, never tools/handles. New
  input branches; branch restore reverses to the common ancestor then applies
  the chosen suffix under the same idle/stop boundary.
- Ctrl-X G shows admitted human inputs, linear except at forks. Enter restores
  before the input; Space inspects; arrows follow nearest human ancestry across
  hidden entries. Archived/blocked inputs stay inspectable. `/branch ID` accepts
  balanced cuts; `/redo` restores the previously selected branch.
- All file tools share apply/restore: capture bytes, mode, absence and created
  parent directories; moves are delete/create. Reject symlinks, special files
  and multiply linked regular files. Outside-workspace effects are marked
  non-undoable; shell writes are never checkpointed. Refuse restoration if
  affected paths differ from expected snapshots. Other runtimes, shells and
  editors are independent writers.
- Loading writable history snapshots its selected ancestry through the last
  balanced main tool exchange into an independent session, sharing immutable
  lineage assets. It preserves source records and retained-input provenance,
  hides old live runtime messages from model input, and establishes an undo
  boundary at the imported tip. Archived predecessors remain read-only. No
  files, tools or live handles are restored.
- File writes use same-directory temporary files and atomic per-file renames.
  Multi-file edits/restores and their SQL commits are independent: partial edits
  record their applied subset when possible; failed restores keep the old
  cursor. Startup never settles unfinished requests, repairs files or replays
  operations. A crash may leave files ahead of saved history; manual loading
  uses saved context.

## Minimal SQLite schema

- Use local SQLite with foreign keys per connection, WAL, full sync and bounded
  busy timeout. The canonical [schema](../internal/history/schema.sql) owns
  table definitions. Entry/change/request IDs are monotonic; other IDs are
  opaque text. Times are UTC Unix milliseconds; typed codecs validate versioned
  JSON.
- Empty databases initialize in one immediate transaction. Current schema is 12;
  startup supports schemas 5–12 and runs ordered, adjacent upgrades (5 → 6 → 7 →
  8 → 9 → 10 → 11 → 12) in one transaction, validating before commit. Each step
  reports its own progress; completion of a step is not a committed upgrade.
  Rebuilding entries temporarily indexes all incoming foreign-key lookups to
  avoid per-entry table scans. Schemas below 5, nonempty unversioned databases
  and unknown newer versions fail explicitly; no automatic resets or credential
  deletion.
- Cooperating runtimes hold a shared data-directory lock for their process
  lifetime; initialization/upgrades require exclusive access. Stop all TTC
  instances sharing that directory before upgrading: pre-lock binaries cannot be
  fenced retroactively. Same-version runtimes may otherwise run concurrently.
- Legacy decoding lives only in `internal/history/migrate`. Migration converts
  retained inputs to current typed messages and legacy marker rows to nonvisible
  status entries, preserving entry/source identities, cursor evidence and
  original input provenance. The 7 → 8 step converts canonical direct and nested
  child completion status records to `outcome` (`completed`, `failed`,
  `cancelled`) and `context_state` (`idle`, `closed`, `unknown`), preserving
  event identities, copied-entry relations and unknown fields. It also converts
  `child_state.child` snapshots to storage-owned `outcome` and `context_state`
  fields. Snapshots additionally permit `running`; missing historical snapshot
  state or outcome becomes `unknown`. Missing completion state becomes
  `unknown`; new completions require observed `idle` or `closed`. Runtime reads
  accept only the canonical format, without old-key fallbacks. Saved original
  provider requests, tool results, model messages and archive files remain
  original evidence; migration does not repair or replay execution. The 8 → 9
  step removes generated compaction archive metadata, preserving frozen cuts,
  original summaries and existing archive files. Archives are no longer load
  prerequisites. Upgrades from schemas 5–8 do not backfill isolated
  `actor_compacted` or `actor_context_prefix` rows: historical child/aside cuts
  are unavailable to `before_compaction`, and historical aside exports contain
  only their own recorded entries, without reconstructing the frozen main
  prefix. Historical main cuts remain available in `compactions`. The 9 → 10
  step adds oldest-entry and authored-prompt indexes and prefixes the session
  listing index with workspace identity. The 10 → 11 step is a version-only
  compatibility fence for the `compaction_context` message vocabulary; it does
  not rewrite historical records or table definitions. The 11 → 12 step builds
  typed request observations from original admission, instruction and completion
  events, then removes the obsolete mutable attempt-diagnostics column. Original
  input JSON, immutable audit events and archive bytes are preserved; copies
  resolve observations through source identities rather than duplicating them.
- History recovery returns typed pending-event data, not model-facing child
  messages. Explicit session DTO projections map `outcome` to
  `assignment_status` and known `context_state` to `child_state`, omitting
  migrated `unknown` state. Live and recovered completion notifications use the
  same projection; recovery never restores live child handles. Child results use
  a typed constructor with explicit fields, not generic job `Snapshot` JSON
  inheritance. Generic job APIs keep their scheduler contract.
- Model preferences live outside SQLite;
  [catalog/startup](models.md#catalog-and-startup) defines file locking, limits
  and selection failure semantics.
- Blank identity/name/selection stays in memory. Startup/new/clear insert
  nothing; first user admission atomically creates
  workspace/session/turn/message with the same runtime ID and empty undo
  baseline. Failure starts no inference or partial rows. Pickers use canonical
  workspace paths, including unregistered workspaces, and list only durable
  sessions.
- `entries.content_json` holds typed messages, origin labels or tool references,
  not duplicate results. Main input follows selected ancestry and
  `model_visible=1`; child records use the same tree with visibility false.
  Status is UI history, never a hidden instruction. Child execution remains
  memory-only.
- `model_requests` records original admission/instruction identities and
  prepared input metadata. `request_observations` records each completion's
  immutable status, optional measured usage and checkpoint, linked to its
  original events. Missing or unusable provider counts are unavailable, not
  measured zero; audit attempts preserve the original feedback without a second
  mutable diagnostic copy. Live accounting and persisted observations use the
  same usability rule. Calibration never crosses a selected compaction boundary.
  Historical decoding belongs to migration, not runtime fallback queries.
- `file_changes.paths_json` is a versioned ordered list of paths, before/after
  absence/type/mode/hash/blob state and actual applied flags. Non-undoable
  effects include reasons/paths. No filesystem-operation journal or recovery
  state.
- Validate same-lineage references, acyclic parents, source IDs and file-tip
  agreement. Compaction copies source IDs/shared change references without
  reapply. Resolve retained checkpoints through source IDs in the continuation;
  if removed, use its summary baseline. Freeze predecessor and insert
  continuation in one transaction. Manual copies can have independent writable
  siblings in the same lineage; active request completions follow their own
  compaction successor chain.
- Use keyset pagination, indexed tool lookup and recursive ancestry/change CTEs.
  Export the latest saved tool record at/before its cut. Persist compact final
  request metadata/usage/response IDs and linked retry notices. Save inspectable
  internal request inputs separately as described above. Session
  lists/inspectors require no full transcript scan.
- Artifacts are private, lineage-owned and referenced by relative paths/hashes;
  deduplicate only within the lineage. No global durable blob table or durable
  job/timer/inbox table. Enforce the
  [retention policy](requirement.md#data-retention) with an atomic activity
  recheck before lineage deletion. The CLI runs maintenance synchronously before
  runtime/frontend construction and gates the 30-day inactive-lineage sweep on
  database-plus-WAL size of at least 2 GiB, without forcing the database below
  the threshold. After deletions, attempt SQLite compaction and WAL truncation
  with a two-minute limit. Compaction requires exclusive lifetime use,
  coordinated under the initialization lock; other open stores defer it rather
  than blocking their ordinary writes with VACUUM. Later startups above the
  threshold retry SQLite free pages/WAL residue without another forced eviction.
  An hourly activity-only heartbeat protects idle loaded sessions from other
  instances' startup sweeps; lifecycle commands also refresh loaded activity.
  Failures appear as UI-only warnings and may occur after history deletions have
  committed. Copies protect shared ancestry/assets. Startup statistics (physical
  database plus WAL bytes, estimated entry count, oldest-entry age) are UI-only
  status, not model input or a new persisted conversation entry. The estimate is
  the surviving maximum entry ID minus minimum ID plus one (zero when empty), so
  deletion gaps can inflate it. Separate indexed endpoint and timestamp lookups
  avoid full-history aggregates. Prompt recall uses a partial authored-input
  index; session lists seek by workspace and activity. Migration and maintenance
  report progress on startup stderr; candidate counts are tentative until
  commit, and phases without known work totals show no percentage. Final
  warnings and statistics use ordinary frontend status events. Signals cancel
  maintenance, but initialization and schema migration do not observe the signal
  context.

## Validation

- Round-trip every tool codec; verify immutable results and stale-handle errors.
- Exercise mixed parent/child edits, idle child writes, branching and conflict-
  checked undo/redo across SQL/filesystem failures.
- Test compaction selection/commit boundaries with live jobs, timers and
  interruption; restart must never relaunch work. Check indexed queries on
  realistic histories.
- Run `make test` and `make check`; use self-contained mock providers and
  headless PTYs, then verify actual Kitty/tmux rendering separately.
  Subscription smoke tests require explicit authorization and a small inference
  budget.
- Exclude daemon mode, durable jobs/timers, adoption, native steering,
  cross-session undo and independent child undo.

## Design basis

The concrete boundaries above are TTC decisions, informed by:

- Anthropic:
  [composable agent boundaries](https://www.anthropic.com/engineering/building-effective-agents)
  and
  [compaction/retrieval](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents).
- OpenAI:
  [reasoning/context continuity](https://developers.openai.com/api/docs/guides/reasoning),
  [stable prompt-cache prefixes](https://developers.openai.com/api/docs/guides/prompt-caching)
  and
  [headless device-code authentication](https://learn.chatgpt.com/docs/auth).

## Terminal rendering ownership

- The Responses adapter counts argument delta events and UTF-8 bytes by output
  index/item ID after call announcement. The completed output item must match
  finalized arguments, name and call ID, without duplicate completion. Emit
  calls in output-index order only after `response.completed`; failed/truncated
  streams execute none. `call_start` is inspectable display metadata, not a tool
  intent. Cap SSE events/ arguments at 8 MiB and retained response output at 32
  MiB. See the
  [streaming contract](https://developers.openai.com/api/docs/guides/function-calling).
- The TUI owns a lazy transcript and indexed block heights. Scroll state keeps a
  block/source-byte anchor separately from follow-tail. Typeset only
  intersecting 16 KiB source blocks; unseen heights are estimates. Resize
  preserves anchors; bounded layout caches serve drawing and hit-testing. Fences
  cross chunks; source mapping is exact for plain wrapping and approximate for
  decorations. Label estimated totals. Ctrl+D at bottom, Esc or submission
  resumes follow-tail.
- Assistant/tool snapshots use one identity-indexed replacement path. Completion
  replaces streamed text with Markdown; finalized objects reject late updates.
  Frontend alone owns transcript, screen and Kitty state. Main assistant labels
  align left and bodies indent two cells. Subagent assistant responses and final
  handoffs use one clipped row with agent name and response prefix; inspect the
  corresponding Request row for its captured full response. Items own no refresh
  goroutines.
- Sidebar sections independently scroll/collapse copied request/job/timer
  metadata; job/timer rows open live inspectors with source-agent names. Timer
  details retain original scheduling parameters and refresh state/countdown on
  frontend ticks. Names use the frontend's durable-label cache; drawing never
  scans history or revives job IDs. A cancelable joined worker refreshes
  optional Git root/branch every five seconds with two-second deadlines and
  bounded output. Workspace header uses `Workspace.Root`.
- `ttc FILE` opens standalone Markdown or an image preview with the shared
  terminal, without a runtime, history, sidebar or composer. Markdown uses the
  inspection-window layout cache and graphics worker, starts at the top and
  shares window scrolling controls. The complete document is parsed together,
  preserving references and containers across source chunk boundaries.
  Redirected Markdown is plain text at 80 columns. Images reuse the pan/zoom
  preview without a Markdown worker or MathJax setup and require Kitty graphics;
  redirected images fail. Format validation and limits are documented in the
  [usage guide](../README.md#usage).
- Markdown layout walks Goldmark's AST and resolves the shared Glamour style
  configuration into styled text and atomic math/image boxes. Greedy paragraph
  flow aligns integer-cell baselines; lists and quotes reserve prefix columns
  before laying out children. All tables share column sizing and cell flow,
  independent of formula presence. Rasterization is separate from terminal-grid
  geometry.
- Local Markdown images share math's box fitting, viewport-only asset work and
  Kitty placement. Paths resolve from the document directory or chat workspace;
  remote URLs remain text. Clicking an image opens the same pan/zoom preview as
  `image_show`, without creating an agent coordinate-selection request. Local
  thumbnail cache recipes use source bytes, not mutable file paths.
- Fullscreen uses a frozen transcript snapshot with shared immutable sources and
  independent indices/anchors. Background work still updates the live view;
  periodic status/asset redraws and automatic dialogs pause. Keep keyboard
  scroll/ resize, all columns and a copy-mode footer; hide sidebar, scrollbar
  and idle composer. Typing reveals an overlay. Disable/ignore mouse events for
  terminal copy. Every exit restores mouse reporting and the live source-reading
  anchor.

### Binary, image and math assets

- Providers announce binary formats and byte limits in model metadata; request
  admission freezes them for tool execution, and attachment selection freezes
  them for snapshots. `binaryinput` shares classification, bounded descriptor
  reads, format validation and original-byte caching between `read()` and
  `@`/`/attach`. Adapters own native encoding.
- Binary files from tools and human attachments are references: absolute
  original path, SHA-256 checksum, MIME type and byte size, but no payload in
  the database. Successful reads and snapshots cache the exact validated
  original bytes under their checksum. Canonical messages retain references
  through history/load/compaction. Inline binary payload records are rejected;
  data URLs exist only in provider transport. The injected `binaryinput.Resolve`
  retrieves cached bytes without reopening the source; on a miss it verifies the
  source and repopulates the cache, outside provider code. The adapter encodes
  resolved originals; unavailable originals become explicit outgoing text
  notices without modifying history or tool association. Invalid references,
  cancellation and cache errors still fail requests. Native image tool outputs
  use backend-default detail, without client preprocessing; document inputs use
  native file parts, without local extraction/conversion.
- Original `image_show` bytes are content-addressed private lineage artifacts.
  Disposable original uploads and derived PNGs share
  `$XDG_CACHE_HOME/ttc/assets` (default `~/.cache/ttc/assets`), a 4 GiB
  file-content cap including recipe references/staging, excluding filesystem
  overhead, and 32 MiB per blob. Cache operations enforce 30-day idle retention,
  not a daemon. Hits refresh idle time; TTL/least-recently-used pruning may
  evict active blobs. All bytes are SHA-256-addressed and deduplicated;
  render-recipe mappings refer to these same blobs rather than storing a second
  payload. Render keys include source, parameters, cell size and backend/lock
  hash. Atomic publication and short-lived cancelable filesystem locks
  coordinate writes/access/pruning across instances. No blob payloads enter the
  database; durable lineage snapshots and MathJax dependencies are outside this
  budget.
- One cancelable frontend worker decodes visible assets and owns optional warm
  MathJax/librsvg work for conversation and Markdown inspectors. Queue/transmit
  only visible rows; cancel offscreen tasks and discard decoded thumbnails. An
  LRU holds up to 128 formula rasters; all retained pixels share 32 MiB.
- [MathJax](mathjax.md) owns setup, engine lifecycle, formula limits and
  rasterization. Initialization hands its pre-warmed Node process to this
  worker; exit cancels/joins both.
- Graphics reuse x/ansi's chunked PNG encoder and Unicode image/row/column
  placeholders. Detect before tcell owns input and serialize graphics/screen
  writes through one TTY wrapper. Positive detection enables tcell v3 RGB output
  even without `COLORTERM`; explicit color disable suppresses it. Input uses
  tcell's built-in one-second inactivity timeout for incomplete escape/control
  sequences; TTC adds no APC filter or timeout. Virtual placements follow cell
  redraws; retain only viewport assets and delete only TTC-owned IDs on exit.
  tmux transfers use DCS passthrough. Window-return notifications from a
  no-output control client trigger full placeholder repaint and visible-asset
  re-upload, independent of terminal focus reporting. The client is
  canceled/joined at exit but counts as attached while live, affecting tmux
  attachment counts, hooks and `destroy-unattached` policies.
- Direct detection queries the terminal. In tmux, bounded metadata reads client
  identity, effective pane passthrough and RGB; Kitty with on/all passthrough
  and RGB needs no reply. Missing identity, passthrough, RGB or failed commands
  get specific warnings. Mixed clients are outside the guarantee: tmux selects
  the current/recent client.
- Bordered previews map cell centers through fit/letterbox/pan/zoom to source
  pixels under [image_show](tools.md#image_show)'s confirmation and
  actor-delivery contract. Child completion waits for its click.
- Reserve one actor interaction before source I/O without holding its mutex over
  decoding/filesystem work. Keep the slot until child consumption; exit clears
  request/reply. Concrete modal/load state uses runtime generations to reject
  stale lifecycle results while preserving compaction previews. Continuation
  replay pins archived live cards with original inspector IDs; child image
  commits use the routing gate.

### Composer and session windows

- The composer soft-wraps without changing text, grows to six rows subject to
  terminal height, and scrolls beyond that cap. Ctrl-J/K scroll its viewport
  down/up without moving the cursor; editing or cursor movement reveals it. Only
  Shift-Enter inserts a hard newline, requiring a distinct terminal key report;
  pasted newlines remain intact. External-editor results retain leading and
  internal whitespace but have trailing whitespace removed.
- Up/Down always recalls original main user/steer messages across saved
  sessions, capped at 1000 entries/8 MiB. Exclude file snapshots, runtime
  notices and continuation copies. Current submissions, including commands,
  share the caps. Ctrl-R searches whitespace-separated case-insensitive
  substrings with AND semantics, in any order, newest first. Queries have at
  most 256 characters; render at most 128 visible result rows with 512-byte
  previews. Fold full prompts once on opening; filter with deduplicated terms,
  longest first and early rejection. Highlight all occurrences in sanitized
  previews, merging overlaps and preserving Unicode positions. Enter recalls the
  exact prompt; Esc preserves draft.
- Slash completion is local. One joined/debounced path worker recursively scans
  at most 10,000 entries below the current or typed directory, without following
  symlink directories; skip unreadable or disappeared descendants. Share
  Ctrl-R's folded substring AND matcher; return up to 256 matches sorted by path
  length, then alphabetically. No persistent index. Accept only matching
  draft/cursor/generation results; snapshot selected attachments asynchronously.
  Paste and dialogs retain key priority.
- Ctrl-X E suspends terminal/graphics around a cancelable `$VISUAL`/`$EDITOR`
  using a 0600 scratch file. Drain runtime events while suspended. Failure
  preserves draft/cursor; valid UTF-8 edits up to 8 MiB replace without sending.
- `/sessions`/Ctrl-X L queries at most 100 workspace-filtered metadata rows, not
  transcripts. Group local activity dates: Today, Yesterday, six prior weekdays,
  then ISO dates. Headers cannot select; click selects, Enter uses idle `/load`.
- `/load` and `--session` copy writable SQLite history before activation,
  without requiring compaction archives. Failed preparation leaves the prior
  runtime usable; read-only history stays immutable. Every coding request uses
  current system instructions.
  [Runtime snapshots](system_prompt.md#runtime-snapshots) define initial
  metadata and per-request refresh. Explicit loads never restore source live
  state. [Compaction-boundary recovery](compaction.md#reload-recovery) copies
  saved undelivered notifications into fresh events owned by the loaded session;
  automatic delivery never acknowledges the source runtime's events.
- Events carry generation to reject stale publications after session changes or
  same-session restore. Metering updates after responses and tool batches. Loads
  replay without an acknowledgment window; new/clear, undo/redo and export use
  brief conversation acknowledgments. Help/compaction/job/timer lists use
  windows; errors stay in conversation.

## Read-only side questions

- `/btw QUESTION` freezes the latest balanced main request while busy, or
  selected canonical history while idle. Model/context freeze at admission;
  shared system guidance plus the `Btw` section in
  [system.yaml](../prompt/system.yaml) constrain the quick answer.
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
  redo. Source inspiration:
  [OpenCode BTW](https://github.com/dalekirkwood/opencode-btw/blob/main/commands/btw.md)
  and [pi-btw](https://github.com/dbachelder/pi-btw); no plugin code is
  imported.
