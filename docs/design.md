# TTC v1 design

TTC is a Go application for Linux hosts. This design defines the complete
first-version target; README.md describes setup and current behavior. TTC uses
one process, one active main session, and SQLite for history and reversible
file edits. [Requirements](requirement.md), [tool contracts](tools.md),
[compaction](compaction.md), and the [system prompt](system_prompt.md) define
the corresponding user and model behavior.

## Boundaries

| Durable across launches                        | In memory for the active session                          |
| ---------------------------------------------- | --------------------------------------------------------- |
| Conversation and history branches              | Running model streams and retry waits                     |
| Tool arguments, results, and Markdown records  | Shell/LSP jobs, child handles, and output buffers         |
| Model selection and request usage              | Timers and pending completion notifications               |
| File-tool before/after states and undo cursors | Queued prompts, steers, questions, and armed image clicks |
| Submitted attachments and compaction archives  | Live child contexts and concurrency slots                 |

Persisting a tool record is recording history, not checkpointing an executable
runtime. Opening a session restores its conversation, never its jobs or timers.
Long-running research work belongs in user-managed tmux. TTC background
commands serve short tasks such as compiling and testing.

There is no daemon, IPC protocol, reconnect service, durable scheduler, or
process adoption. The TUI and runtime share a process; tmux detach leaves that
process running. App exit, `/new`, `/clear`, and loading another main session end the
current live session. Cancel/join its model work, child
agents, shell/LSP process groups, and timers before entering another session.
Opening a child inspector or switching UI panes stays in the same live session.
Compaction replaces conversation context inside the same live runtime. Jobs,
timers, children, queued input, and their IDs remain intact. Only the runtime's
current conversation session ID changes; completion handlers resolve that ID
when committing their next history entry. A routing read lock covers child
history commits and file-tool execution; continuation takes the write lock
before freezing the predecessor. Model streams and blocking non-file tools
do not hold that lock. Each actor uses fixed coding instructions. Requests append inspectable runtime
context for their frozen selection and current environment, including children. No process or timer is restarted.

Graceful shutdown records interruptions and available bounded output. After a
crash, mark unfinished historical calls/turns interrupted; never relaunch them
or fire missed timers. No process cleanup/adoption guarantee is made after an
abrupt OS-level process kill. Commands must remain in their managed process
group; daemonization and long-lived services are outside the shell contract.

## Go module structure

Use one Go module, with these packages and direct dependency construction:

```text
cmd/ttc                  CLI entry point and wiring
internal/session         active session, turn loop, in-memory jobs/input/timers
internal/provider        model/config types, streams, login UI contract
internal/provider/openai subscription adapter and device-code authentication
internal/tool            registry, versioned codecs, dispatch, Markdown records
internal/tool/...        files, shell, web, LSP, questions, children, skills, timers
internal/workspace       serialized edits, undo/redo, minimal filesystem journal
internal/history         SQLite rows, branches, artifacts, Markdown export
internal/context         request projection, budgets, compaction
internal/tui             composer, reusable inspection window, login rendering
internal/jobs            Linux shell process groups and child task lifecycle
internal/capture         shared bounded stdout/stderr rings and cursors
internal/scratch         shared sticky root and verified private UID directory
internal/skills          embedded/local discovery and precedence
internal/render          Markdown, Kitty capabilities, readable plain output
```

Embed `default-skills` through a Go package in that directory. Keep small
interfaces at their consumers; use concrete implementations at construction.
No general plugin framework, service locator, or reflection-based persistence.

AGENTS.md instructions are read root-to-cwd at request boundaries and supplied
when changed. Deeper instructions are read by the agent before scoped edits.
Skill discovery scans user and root-to-cwd `.agents/skill` and `.agents/skills`
directories for `*/SKILL.md`. Exact-name precedence is nearest project, parent
project, user, then bundled; plural paths win over singular at the same level.
Discovery loads metadata, while the skill tool reads the chosen body on demand.

One session loop owns live state; workers send results to it through bounded
channels. Context cancellation follows the live session, with child contexts
for foreground turns and jobs. Interrupting a turn leaves independent jobs
running until the session ends. A pending question yields control to the loop.
At most four child/asides tasks run concurrently; retain at most four coding
child contexts, including idle ones. Children cannot spawn children.
Parent and child requests can overlap; there is no fixed request-cycle limit. A child
uses the same actor-scoped compaction algorithm as the parent. Idle follow-ups
reuse isolated context with a new turn, job and immutable result.

## Event ordering and main timeline

Use one runtime commit path for semantic events from every actor. It assigns
monotonic `event_seq`, actor, causal request/call reference and chronological
main-turn attribution and `undo_owner_turn_id`. The latter refers to the latest
admitted human user turn, not a notification-only inference turn. This is a
small serialized writer/admission gate, not a
plugin event bus. SQLite row IDs identify storage rows; event identity survives
continuation copies. An event copied into another context keeps its source
sequence and does not produce another notification. `Entry.EventSeq()` is the
original `source_id`, or its SQLite `AUTOINCREMENT` ID for an original entry.
Physical copy IDs identify the context projection; timestamps never break ties.
Allocate sequences durably in the commit transaction; restart, branch selection
and retention cleanup never reset the allocator. Sequence gaps are harmless.
The lineage timeline uses event chronology. Actor context instead has an explicit
projection: new summary first, then retained source events, then subsequent work.
Copying old events or moving this context cursor is not a new historical action.
Queued prompts and pending main steers stay transient until admitted; they do
not receive a durable user-turn position merely by entering the composer queue.

Workers report completions to this path. Provider-ordered tool intents commit
before dispatch; terminal results commit once in observed completion order.
Canonical model input reconstructs paired results in original call order.
Streaming deltas and periodic progress only update the corresponding live UI
object; they do not allocate durable events or change committed history order.

Turn admission and complete file apply/journal/history commits share the
workspace mutation gate. Commit the user entry and starting file tip before
releasing it. A child write cannot straddle that checkpoint. An edit's main turn
is the current main inference turn at commit; its undo owner is the latest
admitted human turn. Notification wakes do not create user checkpoints or change
that undo owner. Preserve launch-turn provenance separately. No SQL transaction
spans a model call or filesystem operation. Acquire the workspace admission/
mutation gate before the serialized history commit gate; never make the writer
wait for a worker holding the workspace gate.
The gate orders recorded file effects, not shell/external writes.

Request admission freezes an event cutoff, selects eligible notifications in
sequence order, and commits their delivered IDs with bounded request metadata.
Runtime-context job/timer state and usage counters are projections of the same
committed cutoff; unpublished worker updates cannot alter the frozen input.
Each actor appends runtime context only when its state, project instructions or
observed transitions change. Unchanged requests still record message count, cutoff and
notification acknowledgments. Activation and compaction force fresh context;
failed admission never advances the actor's cursor.
Later events stay queued. Failure leaves delivery pending; retries of an admitted
request reuse its immutable in-memory input. Exact messages stay in canonical
history and archives; request rows store only versioned input counts, never a
second full transcript. Delivery means recorded admission into a request,
through a tool result or notification message, not successful inference or merely
committing a tool record. A foreground finish is associated with its result
and acknowledged at request admission; it cannot schedule a redundant wake.
An eligible idle wake is admitted through the same
gate before queued human input. This defines one linear timeline without claiming
repeatable completion order across independent executions.
Repeating timer coalescing selects the latest committed firing at or before the
cutoff with its cumulative count, in that event's sequence position. Advance the
delivery cursor only through that firing; later arrivals cannot change a frozen
payload or disappear when earlier delivery is acknowledged.

Publish state transitions and notifications together. Child finish commits
terminal turn state, idle/closed status and `child_turn_finished` before another
assignment can be accepted. Successful child compaction commits its actor context
handoff and `child_compacted` together. A foreground result carries the finish
event for acknowledgment at request admission; background enqueues one notice.
No separate `job_exit` is emitted for a child turn.
`/btw` events have UI-only delivery. Session closing rejects new admissions,
cancels/joins workers, records terminal events and discards pending model delivery.

Compaction handoff is another ordered commit. Main handoff changes the current
main conversation and retains committed tails; child handoff changes only that
actor's context cursor. Neither loses queued events nor replays delivered ones.
Child-only cuts never make main history read-only or move its file undo floor.
Never hold SQL transactions or mutation locks while waiting on UI/model consumers.
## Parallel tool execution

Within each completed model response, read-only tools (`read`, `glob`, `grep`,
`skill`, `web_fetch`, `web_search`, `job_list`, `job_read`, `wakeup_list`) run concurrently.
After their results are recorded, file mutations and remaining control tools
run in model call order. Shells and subagents overlap both phases. This is
best-effort ordering within one batch, not isolation from commands or child
writes. Dependent operations require a later response. Foreground calls are
joined before the next model request. Ordinary tool errors do not stop siblings;
storage failure cancels the batch, settles unstarted calls where storage allows,
and joins workers. Interrupted streams do not execute emitted calls. History/UI
record completion order; child model contexts retain original call order.

One workspace mutation queue handles every `edit`, `write`, and `patch` from
parent and children, plus undo/redo. Serialize the complete read/validate/write/
record operation, not just individual writes. Do not hold a SQL transaction
while waiting for a model, process, user, or filesystem operation. One writer
serializes database commits. A process lock on the data root and an exclusive
workspace lock prevent independent TTC processes from bypassing this queue.
Shell commands and external editors do not participate in that lock.

## Provider and model abstraction

Providers own login flow and model knowledge; the TUI or plain terminal renders
typed steps. Providers never import widgets or print directly to the terminal.

```go
// Provider supplies validated models, authentication, and cancellable streams.
// Stream stops if ctx is cancelled or emit returns an error.
type Provider interface {
    Models(ctx context.Context) ([]ModelSpec, error)
    Login(ctx context.Context, ui LoginUI) error
    Stream(ctx context.Context, req Request, emit func(StreamEvent) error) error
    EstimateReplay(message Message) int
}

// LoginUI renders a provider step and waits only when an answer is required.
type LoginUI interface {
    Present(ctx context.Context, step LoginStep) (LoginAnswer, error)
}
```

OpenAI v1 uses **device-code login**. The adapter obtains a code and verification
URL, presents them with expiry, polls at the server-prescribed interval, handles
slow-down/expiry/cancellation, and stores successful credentials atomically in
a private 0600 file. The user can authorize from a browser on another machine.
No server-side browser, callback listener, or API-key fallback is required.
Device-login unavailability is a visible error. Authentication secrets and codes
never enter conversation history, SQLite tool records, or logs. Refresh is
serialized inside the adapter; app exit cancels an unfinished login.

Use the supported subscription transport and validate its actual model/tool
capabilities during integration. Device code settles the login UX; it does not
justify assuming every public API option is supported. Credential reuse is an explicit user opt-in through `--import-codex-auth`; copy
subscription credentials into TTC's private store without changing their
source. Never silently reuse credentials or switch billing mode.

`ModelSpec` contains stable provider/model IDs, display name, available variants,
capabilities, and catalog revision. A variant is a validated preset of model
options such as reasoning effort and an advertised service tier. Store the
initial selection at turn start and the actual immutable selection on each
request. Picker changes apply after the current tool batch, before the next
request, with an inspectable switch message. Existing children keep their
selection. Startup uses the last explicitly selected model ID/variant for the provider in
the data root, resolved against a fresh catalog; without a choice, use the first
catalog entry's default. CLI model/variant overrides are optional. Read-only
inspection does not change the remembered choice. Each resolved model supplies:

| Token field                | Meaning                                                     |
| -------------------------- | ----------------------------------------------------------- |
| `context_limit`            | Effective input-plus-output capacity                        |
| `max_output_tokens`        | Endpoint output ceiling, or zero when not published          |
| `output_allowance`         | Coding output reserve; cap when the provider supports it     |
| `estimation_margin`        | Reserved estimation/wire-overhead safety margin             |
| `recent_tokens_target`     | Desired recent history retained after compaction            |
| `next_turn_input_reserve`   | Free capacity for new input after compaction                 |
| `summary_output_allowance` | Summary output reserve; cap when the provider supports it    |

Providers supply explicit defaults. The subscription catalog does not publish
an output ceiling, and its verified Responses request schema does not provide
an output-cap field. Zero denotes that unknown ceiling; `output_allowance` is
then an application context reserve, not an enforced generation cap. Reject unknown variants, invalid options,
nonpositive context limits/allowances, negative reserves/ceilings, and configurations that cannot
fit fixed instructions plus summary/output/headroom. Do not invent model limits.
With capacity `C`, output allowance `O`, margin `M`, and estimated input `I`,
compact when `I + O + M >= C`. After compaction require:

```text
fixed_input + summary_and_archive_link + retained_history + O + M
    + next_turn_input_reserve < C
retained_history <= recent_tokens_target (except mandatory partial-turn tail)
```

The main loop compacts automatically before an oversized request, after tools
settle. `/compact` shares the same archive and handoff path. A partial-turn cut
retains its initiating user instruction and last two assistant messages with
complete tool results. This mandatory suffix may exceed the desired recent
target; capacity/headroom checks still apply. Pre-cut edits become the undo
baseline. Recheck committed tails under the routing and workspace admission
locks before handoff. The frontend reloads without a popup and preserves
input/live interactions. Main and child compaction share retention, immutable
Markdown/JSONL archives, summarization and fit checks; child cuts never change
main history or undo ownership. Summaries use one request. Non-recoverable
compaction errors disable the affected context; transient network/service errors
and interruption leave it usable. There is no context-rejection retry, summary
regeneration or context-aware tool truncation.

Reserve and retention are different quantities. Estimate images/tool schemas
and label estimates in the UI. `ReplayState` identifies its provider, underlying
model ID and codec version. Its native items replace the canonical assistant
message on the wire and are counted once. Adapters validate their codec against
canonical text/calls, including assistant phase, original item IDs and encrypted
reasoning. Adapters estimate native replay occupancy from model-visible content,
excluding transport metadata. OpenAI encrypted reasoning uses a coarse decoded-size
estimate. These transient estimates never alter replay bytes or reported usage.
Stored JSON arguments are compared after marshaler normalization,
which accounts for RawMessage whitespace/HTML escaping without converting
numbers through floating point. Streamed argument completion snapshots remain
exact string comparisons. `ContextFor` omits foreign provider/model state without changing saved
history. A Standard/Fast switch preserves state for the same underlying model.
Compaction drops replay state. Unsupported native codecs fail explicitly. Reconstruct input from canonical
history, failing explicitly if the adapter cannot represent it.

V1 delivers main-agent steering at the next model boundary; children accept
only idle follow-ups. Native mid-stream steering
and provider-side stream recovery are out of scope. Retry transient errors until
interruption only before any text/tool announcement/native item is committed. Positive
MaxAttempts values bound total attempts; automatic naming uses one. Emit a typed
retry event before each cancellable wait and persist a hidden, inspectable system
notice associated with its request, for parent, child and compaction streams.
Notices do not commit model output; callback errors stop retries. Backoff doubles
from one second with 25% jitter, capped at 30 seconds. Numeric/date Retry-After
overrides backoff with that cap; zero and past dates permit immediate retry.
Keep retry bookkeeping constant in memory. After partial
output, record failure without replay. Restart never retries unfinished model
requests. Store usage and final aggregate metadata on each request record;
individual retry notices retain attempt numbers, reasons and delays. There is no
persistent retry queue. Output counts may include reasoning; do not double-count.

Endpoint-reported `Usage` is independent of context estimates. Optional cache-read, cache-write
input and reasoning output counters preserve unavailable versus zero. Cached
input is a subset of input; reasoning is a subset of output. Uncached input is
total input minus cache reads. The sidebar displays
the latest successful parent response with its frozen model; children, naming
and compaction retain their usage in request records without replacing that
view. Separate run totals sum every finished parent/child/aside/naming/compaction
response exactly once since activation, including repeated cache reads on later
requests. Cache reads/writes are disjoint input subsets, reasoning is an output
subset. Missing request usage is represented by coverage; optional totals remain
unavailable when any reported response omits that counter. UI snapshots copy
all counters under the runtime mutex. Compaction retains totals; explicit
activation clears them. Rates vary by producing model/tier; no dollar bill is
computed from mixed-model token totals. Context estimates and reported counters carry producing request IDs.
The sidebar uses reported input only for the matching request, with reserves
and component estimates labeled separately; percent and numerical usage include
reserves. A compaction handoff immediately estimates the replacement context;
the last reported response remains visible separately. A new request may estimate
another model while the last reported counters remain visible. Explicit session
changes clear both. No subscription allowance
or cost is inferred from these counts.

## Inspectable request messages

Every committed conversation message is inspectable. Save the exact system
instruction snapshot for each coding, naming, and compaction request in a private
lineage artifact. The TUI keeps one coding placeholder per actor, pointing to its latest snapshot;
internal naming/compaction prompts remain distinct. Clicking that placeholder
opens the same scrollable `tui.Window` used for messages, tools, and command
results. Plain output exposes it through `/inspect <entry-id>`.

Internal naming and compaction inputs/replies have inspectable history records
outside the coding model context. Authentication tokens, device codes, and
login UI steps are never conversation messages or inspector artifacts.
Native encrypted reasoning items stay in versioned provider state and are
preserved in exact JSONL exports. Markdown omits opaque state and uses brief
labels for internal requests, avoiding duplicated summarizer input. A
continuation drops provider replay state from coding requests.

## Tool codecs and shared Markdown

Every tool implements serialization/deserialization for its validated call and
historical execution record. SQLite stores a tool name, version, and encoded
payload. Decoding never starts execution or restores a live handle.

The [tool package](../internal/tool/tool.go) defines the codecs: `Tool` decodes
validated `Call` inputs and concrete `Record` values. A call executes to a result
or error; the dispatcher records exact arguments, model JSON and Markdown
together. Unknown versions fail explicitly; records contain no live resources.

The dispatcher supplies `Execution`: stable IDs, cancellation, and narrow tool
services. A call intent commits before execution, followed by its exact result
and record. Validation errors use a common dispatcher record with the original
input retained as data. A job-returning result is immutable even after that job
finishes. Completion appends a new historical record/entry; it does not rewrite
what the model previously received. At restart an unresolved foreground call
gets one interrupted result, allowing well-formed canonical history without
rerunning the call. A returned background job ID remains historical and cannot
be used as a new live handle.

`render.Markdown` supplies a bounded portable Markdown briefing, normally rendered as one
clipped terminal row with highlighted commands/arguments and short labeled
output excerpts. File mutations add up to six short applied diff lines; inspectors use paths,
highlighted snapshot diffs, and labeled parameters/results. Exact JSON stays in
records/sidecars. Glob
shows its pattern alone; status/errors remain visible for other briefings.
Tools may publish transient updates through `Execution.Update`; foreground
shells/children sample bounded capture tails every 250 ms. These replace one
UI card and refresh its live inspector, without accumulating history records.
Final results and separate background completion cards are immutable and durable. Use it directly in the CLI renderer,
inspector, compaction archive, and session export. The plain terminal renderer
uses the same Markdown document. Escape untrusted Markdown values, filter
terminal controls, and choose safe code fences. UI click IDs remain metadata.
Store the Markdown revision with each historical record so exports do not
depend on a future renderer. Exact exports also include original arguments,
model results, provenance, and truncation markers in a JSONL sidecar.
Tool Markdown can provide an optional export body; absent an override, export
delegates to its inspector presentation. System prompt details have an empty
Markdown export projection and remain exact in JSONL/inspectors.

Live shell and child output uses 64 MiB per-call bounded rings, split into
32 MiB stdout and stderr rings under a shared 1 GiB runtime capacity pool.
Pool pressure evicts the least recently written other ring. Default previews
show at most 10 lines or 1 KiB combined; job_read pages a named stream and
accepts EOF-relative byte/line cursors and bounded page grep.
Persist inspection tails of up to 8 KiB per stream on completion/stop; earlier
output stays in live rings and is lost when the runtime ends. Large persistent
details and submitted attachments use private managed files. `/export <path>` freezes a committed
history cut and emits dense Markdown, an exact `.jsonl` sidecar, and a
sibling assets directory with relative links; reject existing targets. User exports are outside retention cleanup.

## Serialized edits and shared undo

Serialization gives all parent/child edits a single order. It does not make
interleaved dependent edits independently undoable. V1 therefore has **one main
session undo history**, including both foreground and background child edits.
Children can inspect their output; they cannot restore a separate workspace
branch. Their edit cards identify the originating child and tool call.

Every committed file-tool change appends an entry to the main session history
and advances its file-change tip. Child messages/tool details are tagged with
the child ID and excluded from the main model context, while remaining visible
in history. Each main user turn starts at a saved history/file tip. Child edits
while the parent is idle extend the most recent human turn's undo suffix. Once a
new human turn begins, later edits fall in that suffix regardless of which child was
launched earlier. The shared admission/mutation gate in
[event ordering](#event-ordering-and-main-timeline) resolves checkpoint races.
Show this chronological grouping in history; it is not a
claim that the main turn authored every included edit.

`/undo` requires the main turn to be idle. Stop/join remaining children and jobs,
clear pending timers/inputs, and discard child handles before restoration.
Reverse every recorded change after the last user turn's starting checkpoint,
including child and trailing async edits, in reverse order. `/redo` restores
that saved suffix in forward order. A new prompt after undo branches history.
Redo and history branch selection use the same idle/cancel/join boundary and
discard live child handles before touching files. Selecting another history
branch restores the old suffix back to the
common ancestor, then the new suffix forward. Never selectively undo one child
while retaining later dependent edits. Redo restores recorded bytes, not tool
execution, jobs, or child contexts. An interrupted/failed main turn is also an
undo boundary once its work is stopped; its applied edits must remain undoable.

Ctrl-X G projects the immutable tree onto admitted human inputs, excluding
runtime notices and child/tool entries. Enter restores the checkpoint immediately
before that input, matching `/undo`; Space inspects the input. Navigation follows
the nearest human ancestor across hidden entries. Archived and blocked inputs
remain inspectable. `/branch ID` still supports explicit balanced history cuts.
After a branch selection, `/redo` restores the previously selected branch.

All three file tools use the same mutation service for apply/undo/redo. Record
before/after bytes, mode bits, absence, and newly created parent directories.
Represent a move as delete plus create. Reject symlinks in mutation paths,
special files, and multiply linked regular files in v1. Outside-workspace edits
are allowed but marked non-undoable. Shell changes are never checkpointed.
Before restoration compare affected files to recorded expected states; refuse
conflicts without overwriting a formatter's or external editor's changes.
The workspace lock cannot prevent external writers racing this check.

Use a monotonic workspace generation to detect intervening edits by another
TTC session. Normal session loading does not restore files. If the stored
session generation differs, open its history with an explicit undo boundary at
the current tip; new work can be undone, but earlier branches cannot restore
across that boundary. A fresh baseline starts from current files. This avoids
cross-session snapshot dependencies and selective merge machinery. Within the
active session, branches and undo/redo update its observed generation together.

One small filesystem journal remains necessary: SQLite cannot atomically commit
filesystem writes. Prepare durable before/after blobs and an operation manifest,
then apply changes through same-directory temporary files and atomic per-file
renames. Fsync files/directories; multi-file patches are not atomic. Finalize the
history/file tips only when the operation's actual outcome is recorded. A failed
patch records its applied subset as a reversible partial change. Unexpected
failure during restore leaves its old cursor and blocks new mutations.

On startup inspect the single pending operation: if disk matches the recorded
before/after states, finish recording an apply's actual subset or finish an
unambiguous restore. Otherwise report conflicting paths and keep edits blocked
until resolved. Never rerun shell/model work. Keep the journal small and local;
no generic workflow recovery engine is needed.

## Minimal SQLite schema

Use a local SQLite database, foreign keys on every connection, WAL, full sync,
and a bounded busy timeout. An incompatible or nonempty unversioned schema resets all data-root contents except the held process lock.
Close SQLite first, retain the lock inode through initialization/recovery, and
propagate deletion errors. This discards history, assets, caches and credentials;
no migrations or preserved-data compatibility paths exist. Empty new databases
initialize normally. Compatible databases and I/O/corruption errors do not reset. IDs are opaque
text except monotonically allocated entry/change/request IDs. Times are UTC
Unix milliseconds. Versioned JSON is validated by typed codecs. Managed artifacts
live under a retention lineage directory; JSON references relative paths and
hashes. Deduplicate within that directory only. There is no global blob-reference
or durable job/timer/inbox table.

The canonical [schema](../internal/history/schema.sql) defines ten tables for
durable history and file restoration.

Model choices live independently in private, versioned `model-choices.json`:
only the model ID and variant per provider, resolved against a fresh catalog at
startup. Selection saves the choice before queuing application; preference
failure rejects it. SQLite switch records and preference updates are separate: a
later record failure preserves the active model while retaining the accepted
startup preference. Preferences are bounded to 64 KiB and fail on invalid input.

Blank conversation identity, name and selection belong to the runtime. Startup
and /new or /clear insert no session row. The first user submission atomically
creates the workspace/session, first user turn and message with the existing
runtime identity and an empty undo baseline. Failure leaves no partial rows and
does not start inference. The frontend reads runtime metadata/entries, so blank
views need no database record. The session picker queries canonical workspace
paths, including unregistered workspaces, and lists only durable sessions.
`entries.content_json` contains typed message blocks, origin labels, or a tool
reference; it does not duplicate the full tool result. Main model context walks
the selected ancestry and filters `model_visible=1`. Child history uses the same
ordered tree with `model_visible=0`; child execution state is memory-only.
A status entry is UI history, never a hidden model instruction.

`file_changes.paths_json` is a versioned ordered list of relative path,
before/after state (absence/type/mode/hash/blob path), and actual applied status.
Non-undoable changes carry the reason and affected paths. The journal manifest
adds operation IDs, old/target cursors, expected generation, and per-path progress.
These payloads are loaded as a unit, so a table per file state/path is unnecessary.
Application transactions validate same-lineage references, acyclic existing
parents, compatible source IDs, and agreement between file tips and records.
Compaction copies entries with immutable source IDs and shared change references;
it never copies or reapplies a mutation. Resolve a retained turn's starting
checkpoint through `entries.source_id` in the current session, rather than
traversing into predecessor history. If that checkpoint was compacted away,
use the summary's baseline entry/file tip. Set predecessor read-only before inserting
the continuation in the same transaction to satisfy `writable_lineage`.

Use keyset pagination on entry IDs and session activity, indexed tool-call lookup,
and recursive CTEs for selected ancestry/change suffixes. Render the latest
record for a tool at or before the export cut, not a mutable current card.
Persist compact final request metadata, provider usage, response identifiers,
and separate request-linked retry notices; do not copy full prompts into every
request row. No full transcript scan is needed for the session list or inspector.

Lineage-owned files avoid cross-lineage retention dependencies. Delete inactive
lineages as specified in requirements, with foreign-key checks deferred for the
single row-deletion transaction. A small deletion marker file allows retrying
filesystem cleanup after a crash. Pending `fs_operation` protects its lineage.
Each retained continuation keeps predecessor data needed by its source entries.
Credentials and workspace generation rows are outside conversation retention.

## Implementation and validation order

1. Build history/tool codecs, file mutation serialization, and conflict-checked
   undo/redo with a fake provider. Verify SQL and filesystem failure boundaries.
2. Integrate the OpenAI device-code adapter and model catalog; verify actual
   subscription access before claiming live provider support.
3. Add the terminal loop and in-memory background jobs, children, questions, and
   timers. Exercise cancellation on exit/switch and stale-handle errors.
4. Add compaction/export and Kitty rendering against the same stored Markdown.

Validate codec round trips for every tool, mixed parent/child edit order, idle
child edits, undo/redo after branching, and immutable delivered results. Exercise
compaction's archive/commit sequence with live jobs and timers and interruption on restart. Check
indexed query plans with realistic histories. Once Go exists, run gofmt, tests,
and vet; verify headless plain output and Kitty/tmux separately. Defer daemon
mode, durable jobs/timers, process adoption, native steering, cross-session undo,
and independent child undo until explicitly needed.

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

### Terminal rendering ownership

The Responses adapter owns argument assembly: output_item.added announces a
function call; argument deltas append only to its matching output index/item ID.
Arguments-done and item-done snapshots must match the accumulated JSON object,
with unchanged name/call ID and no duplicate completion. Validated calls emit in
output-index order only after response.completed, preserving mutation/control
order when parallel calls finish in another order. A separate typed
call_start is display metadata; only complete ToolCall values enter history
intents. The runtime executes calls after the response stream settles successfully.
Truncation, invalid lifecycle/identity or disagreement fails the response without
execution. SSE events/arguments are capped at 8 MiB, retained output at 32 MiB.
See the [official streaming contract](https://developers.openai.com/api/docs/guides/function-calling).
The runtime saves announcement metadata as inspectable status, without adding
model input. Its request-scoped view key becomes the committed tool-card identity.

The TUI owns one lazy transcript and its indexed block heights. First-line state
and a block/source-byte anchor distinguish scrolling from follow-tail. Only intersecting
16 KiB source blocks are typeset; unseen heights are estimates. Resize invalidates
layout while retaining the anchor. Cached layout is bounded to a working set,
and drawing and hit-testing use the same visible rows. Code fences continue over
chunk boundaries. Source mapping is exact for plain wrapping and best effort for
Markdown decorations; estimated totals are labeled. Ctrl+D at the bottom resumes
follow-tail, as do Esc or a new submission.

The sidebar consumes a copied latest-parent-request token estimate and current
job/timer metadata. It does not scan saved conversations on redraw or turn stored
job IDs into handles. Sections own independent collapse/scroll state. The
fullscreen view hides the sidebar and idle composer while keeping the anchor.
It disables mouse reporting and ignores queued mouse events, allowing the
terminal to select/copy text. The conversation uses every column without a
scrollbar or scroll-position decoration. Fullscreen takes a separate transcript snapshot sharing immutable sources and
rows, with independent layout/scroll indices. Background publications continue
in the live view; periodic status/render redraws and automatic dialogs pause.
Keyboard scrolling and resize render the frozen snapshot. Exiting restores live
contents and its source reading anchor; every exit restores mouse reporting.
Assistant and tool events publish current presentation snapshots through one
identity-indexed replacement path. Completion replaces streaming text with
Markdown in place; finished objects reject late updates. Only the frontend owns
transcript, screen and Kitty state. Assistant labels align left; bodies indent
two cells. No item owns an independent refresh goroutine.
Its compact workspace header uses Workspace.Root; one cancelable, joined worker
refreshes optional Git root/branch metadata every five seconds, with two-second
deadlines and bounded output. The UI performs no Git subprocesses during drawing.

Original image_show bytes are content-addressed private lineage artifacts.
Derived thumbnails and MathJax PNGs live in a separate render-cache directory,
with a 256 MiB limit and 30-day idle pruning. Source snapshots are never cache
entries. Keys include source identity, rendering parameters and math backend
version/revision. One bounded, cancelable frontend worker owns image decoding and
optional MathJax Node worker and librsvg subprocesses. Conversation and Markdown
inspection windows share it; failures show warnings and labeled literal TeX.
Only assets referenced by visible rows are queued or transmitted. Offscreen tasks
are canceled and decoded thumbnails discarded. A memory LRU retains up to 128
formula rasters, serving layout hits immediately; formulas and visible thumbnails
share a 32 MiB pixel-memory budget. Math initialization runs independently, then
transfers one pre-warmed process to that worker; exit cancels/joins initialization
and Node.
Each formula gets fresh parser/document state while the output engine/fonts stay
warm. Base TeX, AMS and local macros are enabled; dynamic TeX loading is disabled.
First initialization installs MathJax 4.1.3 via npm ci using an embedded exact
lockfile with SHA-512 package integrity values. The installer has a two-minute
deadline, disables scripts, stages and validates before publishing a private
version/lock-addressed XDG cache directory, and uses a cancelable per-user lock.
Ready packages need no npm/network. The executable contains no dependency archive.
Cache keys include lock and backend hashes. Node hooks are removed and imports
use absolute cached paths. Formula input is limited to 4096 bytes and each render
to ten seconds. JSON-line replies/SVG/PNG captures are bounded to 32 MiB, stderr
to 4 KiB and logical formula dimensions to 4096×1024 pixels. Physical rasters use
three pixels per logical pixel on each axis, capped at 16 megapixels. Interrupted
or broken workers are joined; the next visible formula restarts one. Ordinary TeX
errors keep it warm.

The graphics module reuses x/ansi's chunked PNG encoder and explicit Unicode
row/column/image-ID placeholders. Detect before tcell owns input, then serialize
all graphics writes with tcell through one TTY wrapper. Placeholders keep exact
RGB IDs even when SSH/tmux omits COLORTERM: positive detection
adds RGB to a private terminfo copy. Explicit color disable suppresses graphics.
Virtual placements follow cell redraw/scroll. Only viewport assets remain in terminal storage;
shutdown deletes only TTC-owned image IDs. tmux transfers use DCS passthrough.
Direct detection queries the terminal. In tmux, a bounded metadata command reads
the protocol-reported client identity, effective pane passthrough and RGB feature.
A Kitty client with passthrough on/all and RGB enabled needs no graphics reply.
Missing identity, disabled passthrough, missing RGB and command failures produce
specific warnings. tmux selects its current/recent client; heterogeneous attached
clients are outside this detection guarantee.

Image previews share the bordered window frame and map cell centers through
fit, letterboxing, pan and zoom to source pixels. Selection and OK confirmation
are separate. Runtime pending-click ownership is per actor, separate from saved
results; confirmation/cancellation routes to that actor's next model boundary.
Children wait for their own outstanding click before final completion. Explicit
session changes and exit discard interactions; compaction preserves them.
An actor reserves its one interaction before source I/O, without holding the
interaction mutex during decoding or filesystem operations. The slot remains
reserved until a child consumes its reply; child exit clears both request/reply.
The frontend owns a concrete modal state and asynchronous preview-load record.
Runtime generations change on explicit lifecycle resets, preserving pending
previews across compaction and rejecting results from discarded live state.
Continuation replay pins copied live image cards that were archived out of the
retained suffix, with their original inspector entry IDs. Child image publication
routes under the continuation lock, so it cannot target a discarded predecessor.


Composer completion filters slash commands locally. A single joined, debounced
worker reads one directory in batches, bounded to 10,000 scanned entries and
256 matches. Results are accepted only for the same draft/cursor/session
generation; completion does not recursively index the workspace. Selected files
snapshot asynchronously before submission. Paste/dialog keys retain priority.
Ctrl-X E suspends/resumes the terminal around a cancellable external editor
using a private 0600 scratch file. Runtime events keep draining; no terminal or
Kitty writes occur while the editor owns the terminal. Failure leaves the draft
and cursor intact; successful UTF-8 edits (up to 8 MiB) replace without submitting.


The session picker (/sessions or Ctrl-X L) reads at most 100 metadata rows,
filtered to the workspace,
without inspecting transcript artifacts until reload. Dates use local activity
time (Today, Yesterday, previous six days as weekday names, then ISO dates).
Headers are not selectable. Enter runs the same idle /load lifecycle; click
selects a row. Both /load and CLI --session preflight a writable session's active
branch: a main coding instruction snapshot at least one hour old is reassembled
from current instructions and recorded without inference or clearing redo.
Refresh failure leaves the previous live runtime usable. New requests send the
same current instructions; target archives are validated before canceling live
work or changing target metadata. Dynamic project and
live context is reread per request. Read-only history remains immutable.
Presentation events carry the runtime generation, rejecting delayed events after
same-session undo/redo as well as session changes. Metering refresh notices are
emitted after model responses and settled tool batches, not only turn completion.
Successful loads display the loaded conversation without an acknowledgment
window. New/clear, undo/redo and export keep brief acknowledgments in the
conversation. Help, compaction and job/timer lists open content windows;
errors remain visible in the conversation.

## Read-only side questions

`/btw QUESTION` admits a managed background child with the latest balanced main
request input while work runs, or selected canonical history when idle. Context
and model freeze at admission. The stable coding system prompt is reused and a
short developer message narrows the task to a quick read-only answer. The same
child loop and tool batch executor use a filtered registry for both advertisement
and dispatch; shell, file mutations, interaction, timers and child creation are
unavailable. Each aside has its own actor/cache identity, hidden inspectable
records, bounded final answer (64 KiB), and supervisor captures. It shares the
four-child limit, survives compaction, and is cancelled/joined on explicit
session changes or exit. Answers never enqueue parent wakeups or model-visible
messages. Completion saves Markdown and opens a reusable window, deferred while
a dialog, editor, paste or fullscreen copy view owns focus. Pending popups cap at
16; older answers stay in history. Admission rejects blank/read-only sessions
and pending redo, preventing a supposedly read-only question from silently
discarding redo. Child ownership still limits job visibility; timer listing is runtime-wide.

The design follows the short side-question instruction in
[OpenCode BTW](https://github.com/dalekirkwood/opencode-btw/blob/main/commands/btw.md)
and the enforced read-only context-sharing mode in
[pi-btw](https://github.com/dbachelder/pi-btw). No plugin code is imported.
