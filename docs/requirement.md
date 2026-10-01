# TTC requirements draft

TTC is a coding agent for headless SSH/tmux sessions in a container on Linux
hosts only. README.md tracks implemented behavior; this document describes the
complete target. The
runtime owns work; the TUI displays it in the same process. Kitty renders Markdown and math. The
input box stays at the bottom while responses stream. An LLM adapter supports
multiple providers; OpenAI subscription is the initial provider. It maps
provider-neutral messages, tools, streaming output, usage, and cancellation to
each provider. Provider capabilities include image input; main-agent steering
waits for a model boundary;
switching providers uses stored conversation history, not another provider's
response ID. Provider-owned model catalogs describe names, variants, capabilities, context
limits, output limits, recent-history retention targets, and next-turn headroom.
OpenAI login uses device codes. Provider-owned flows emit typed prompts and
status for TUI or plain-terminal rendering;
credentials never enter conversation history. See [design.md](design.md) for
provider interfaces, immutable request configuration, and persistence.
The system prompt template and its runtime variables are in
[system_prompt.md](system_prompt.md).
Keep coding-turn tool definitions stable for cache reuse. To forbid tool
calls temporarily, use a provider's hard no-tools setting when supported;
otherwise omit the tools. Do not simulate disabling by returning tool errors.

Store the resolved provider, model, variant, and budget configuration on each
turn when it starts. A model
picker change during work applies after the current tool batch settles, before
the next request, and appends a persisted switch message. Existing requests and
children retain their selections. Catalog-advertised Fast choices share the base
model and reasoning variants, with an explicit service tier. Record each model
request retry notice, response ID, timing, token usage, and final status under its
turn; retries use the same provider and model, with no silent model fallback.
The adapter retries transient transport failures, rate limits, and server
errors until interruption by default, honoring numeric/date `Retry-After` with
a 30-second cap and otherwise using bounded exponential backoff with jitter.
Show a clickable system notice before each retry wait, with the next attempt,
reason and delay. Keep notices out of model input. Esc, Esc cancels a pending retry as part of
turn interruption. Retry the same request only before any output or tool call
is committed. After a partial stream, keep the partial output and mark the turn failed. V1
does not resume provider streams or retry interrupted requests after restart. Preserve compatible opaque provider continuation items, including
reasoning items needed across tool calls, separately from visible text. Never
reuse them across an incompatible model, branch, or compaction boundary.
Never replay an executed tool call. Persist attempt state so a restart
does not blindly resend an uncertain request.

Compact long model contexts automatically near the selected model's limit,
or manually with `/compact`. Main compaction starts a new continuation session
named `{original_session_name}-cont-0`, then `-cont-1` and so on. Its visible
conversation starts with the assistant's compaction summary and retained
recent messages, exactly as sent to the LLM. The predecessor becomes
read-only. Preserve full earlier history in SQLite and managed storage, and
give the LLM a searchable, branch-specific transcript path in that visible
summary. Child compaction uses the same algorithm on its actor context without
switching main history. Summary generation is single-pass; input that cannot
fit fails explicitly. See [compaction.md](compaction.md) for handoff and recovery.

## LLM-facing tools

| Tool                 | Behavior                                                                                       |
| -------------------- | ---------------------------------------------------------------------------------------------- |
| `read`               | Read a file or list one directory, with paging.                                                |
| `glob`, `grep`       | Find paths and search text with bounded results.                                               |
| `web_fetch`          | Fetch an HTTP(S) page as readable text, with source URL and truncation status.                 |
| `web_search`         | Search the web; return a bounded list of titles, URLs, and snippets.                           |
| `shell`              | Run a command in a chosen directory, foreground or background. Return job ID and status.       |
| `edit`               | Replace exact text in one existing file; require one match unless `replace_all` is set.        |
| `write`              | Create or fully overwrite one text file from a path and complete contents.                     |
| `patch`              | Add, edit, move, or delete files with one structured patch.                                    |
| `image_show`         | Display a local image inline, optionally requesting one click; return its ID and dimensions.   |
| `question`           | Ask one or more single-choice or free-text questions; return answers or cancellation.          |
| `skill`              | Load a named project, user, or embedded `SKILL.md` into the LLM context.                       |
| `lsp_query`          | Query a background LSP shell job for definitions, references, hover, or symbols.               |
| `subagent`           | Start a child agent, wait or run it in the background, and send follow-ups by child ID.        |
| `job_list`           | List command and subagent jobs with IDs, owners, and states.                                   |
| `job_read`           | Read retained command output or a child agent's status and result.                             |
| `job_stop`           | Stop a command process tree or cancel a child agent.                                           |
| `wakeup_schedule`    | Schedule a one-shot or repeating session wakeup by time or delay.                              |
| `wakeup_list`        | List wakeups, next fire times, and last results.                                               |
| `wakeup_cancel`      | Cancel a wakeup by ID or name.                                                                 |

The exact LLM-facing inputs, results, and tool notes are in [tools.md](tools.md).
A bundled LSP skill gives server startup and workspace setup recipes. The
agent starts the chosen server with
`shell(background=true, protocol="lsp")`, then calls `lsp_query(job_id, ...)`.
For an LSP job, TTC reserves raw stdin/stdout for JSON-RPC, disables the
PTY, keeps stderr as job output, and performs initialization and file sync.
`job_read` never consumes its protocol stream. The skill guides use; the
backend owns protocol correctness and process cleanup.
Compile `default-skills` into the Go binary and discover its skills without
requiring those files on disk. Project skills override user skills, then
embedded defaults. The bundled LSP skill covers language-specific setup.
TTC runs inside a container and does not ask for tool permissions.

## Scratch experiments

Before the first agent request, create `/tmp/ttc/{user_id}`, with
`user_id` set to the process's effective numeric OS UID. Create `/tmp/ttc`
as a shared 1777 directory (world-writable with the sticky bit), which may be
owned by another user. Create the per-user directory as 0700 owned by that UID;
reject a per-user path owned by another UID, a symlink at either level, or
unexpected permission bits. Set permissions on newly created directories
independently of umask; do not repair unsafe existing paths. Provide the absolute path in the LLM
runtime context. Before each scratch use, reverify the path and recreate it
if `/tmp` cleanup removed it; fail the operation if it was replaced by an
unsafe path. Ask the agent to use it as the `shell` working directory for
one-time scripts, probes, and other disposable experiments.
Requested project edits stay in the workspace. Scratch files are ephemeral,
outside SQLite conversation storage and file undo/redo; do not put archives,
attachments, or retained tool-output artifacts there. If the directory cannot be created or
verified, report the error instead of sending an unsafe scratch path.

## Async behavior

### Event ordering and main timeline

One serialized runtime writer commits semantic events from every actor: user
input, model requests/replies, tool intents/results, job transitions, timers,
interactions, model changes and compaction. Each committed event has a monotonic
`event_seq`, actor, causal request/call ID, chronological main-turn ID and
`undo_owner_turn_id` identifying the latest admitted human user turn.
Allocate sequences durably; never reuse them after restart, branch changes or
retention cleanup. Gaps are allowed.
Sequence order governs history and delivery; wall-clock timestamps are display
metadata. Parallel execution may finish in any order, which the writer records
once. A worker never chooses its own history position or main-turn ownership.
Queued human input remains transient until promoted to a turn or admitted as
main steering; promotion, not enqueue time, places it in durable history.

Record tool intents in provider call order before dispatch. Each intent has one
terminal result; a failed or interrupted stream executes none of its calls.
Tool results commit as they arrive, while each actor's model input restores
the response's original call order. Streaming deltas, progress and redraws are
transient views of a sequenced request/call, not separate durable history entries.

User-turn admission, its starting file checkpoint, and complete file-tool
apply/record operations share one admission/mutation gate. Whichever commits
first determines whether an edit belongs before or after that checkpoint.
Idle edits extend the last admitted human turn's undo suffix; launch-turn IDs
retain provenance only. Notification-only inference turns add no user checkpoint
and do not change undo ownership. Other semantic events use the same admission
order. Shell/external effects remain outside file undo and must not be presented
as serialized edits.

At each request boundary, freeze a committed event cutoff. Append eligible
notifications in sequence order and record their delivery together with request
admission; later events wait for the next boundary. A failed admission consumes
nothing. Delivery is inclusion in the persisted request, through either its
tool result or a notification message, not just writing a result record or
successful inference. Notification wakes use the same turn-admission gate and
never start a second main turn concurrently. Event copies preserve source
identity and delivery state; they are not new events and never notify again.
Session switch/exit closes admission, cancels and joins workers, records their
terminal states, and discards undelivered notifications before activating a new
runtime. No old event is delivered across that boundary. Context projection
explicitly places a new summary before retained source events; it is not a sort
of global commit sequences. The lineage timeline keeps original chronology and
records the handoff once.

### Input and background work

V1 has one process and one active main-session runtime. Parent and child model
calls may run concurrently; file mutations share one serialized mutation queue.
A response batch runs read-only calls together, then ordered mutations/control
calls. Shells/subagents overlap both; ordering is best-effort and dependencies
require subsequent responses. Turns and child tasks have no fixed cycle limit.
Human instructions have a distinct background; system messages use a distinct
foreground. Queued prompts appear beneath the working indicator, one clipped
line per message above the composer. Viewers have borders and scroll indicators;
the normal conversation has a scroll-position indicator and scrollbar.
Fullscreen copy mode omits both, disables mouse capture/click actions, and keeps
keyboard scrolling; the terminal handles text selection.
SQLite stores conversation/tool history and file undo data. Jobs, timers, child
handles, queued prompts, steers, pending questions, image clicks, and output
buffers live in memory only. A persisted tool result containing a job ID does
not make that job durable.

- **Enter while idle:** start a turn. **Enter while busy:** enqueue FIFO. Show
  queued input immediately but exclude it from model context until promoted.
- **Alt+Enter while the main agent is busy:** steer at the next model boundary
  after the current response and foreground tools settle. Keep the TTC turn active until
  the steer is handled. Native mid-stream steering is outside v1.
- **Esc, Esc:** interrupt the active request and foreground tools. Independent
  background jobs keep running in the same live runtime. Ctrl+B moves running
  foreground shells to background jobs; pending calls return their job IDs.
  A shell that finishes during the move returns its completed result once.
  `/background` opens the live shell manager; no foreground shell is a no-op.
- **Background shell completion:** update status and persist a bounded historical
  record. With `wake_on_exit=true` (default), enqueue a notification for the
  next model boundary or start a turn if idle. With false, update the UI only.
  Foreground completion already returns a tool result; do not notify twice.
- **Question:** suspend only the asking turn/child; keep the composer usable.
  Present each question in a tab, with Left/Right navigation and a final Submit
  tab. Each question accepts one option or one free-text answer. Up/Down focuses
  choices; Enter selects and advances to the next tab, including Submit after
  the last question. Space selects without advancing. An optional
  recommendation names an existing choice, highlighted without selecting it.
  Every question offers Other for free text. Drafts and selections persist
  across tabs; custom text and options are mutually exclusive. Only the final
  Submit button sends the complete round. Esc leaves custom editing, then
  dismisses the form without cancelling it. `/questions` reopens pending forms.
  Composer Enter queues a later turn. Pending forms are not restored on restart.
- **Wakeup:** deliver at the next model boundary or start a turn when idle,
  ahead of queued Enter prompts. Coalesce a repeating timer to one outstanding
  notification. There is no missed-wakeup replay after exit or session change.
  Coalescing selects the latest immutable firing at or before the request cutoff
  with its cumulative count, ordered by that event's sequence. Acknowledgment
  advances only through that firing; later firings remain pending and never
  mutate an admitted request.
- **Image click:** return immediately from `image_show(request_click=true)`.
  Allow one pending click per actor. The next primary-button click inside the
  displayed image selects a point; explicit OK confirms it and Esc cancels it. Keep the exact displayed image
  snapshot for history, but do not restore the pending interaction on restart.

### Child agents

Coding children have a stable child ID and a new turn/job ID for each assignment.
Only the main agent can assign follow-ups, and only while that child is idle.
Running children accept neither steering nor queued follow-ups. Reject a busy
follow-up with guidance to wait for `child_turn_finished`. A successful final
response becomes idle only after its foreground tools and pending interactions
settle. Failure or cancellation closes the child; start a fresh child for recovery.
Retain at most four child contexts, including idle ones; closing a child frees
its slot. Children cannot spawn children. Child inspectors are read-only.

Commit each turn's terminal state and one `child_turn_finished` event before
accepting a follow-up. Deliver this event to the main agent at the next ordered
request boundary, or admit a notification turn when the main agent is idle.
The event identifies child, turn, job, status and immutable final reply/error
reference. Foreground tool results carry that event for acknowledgment at request
admission; otherwise enqueue one notification. Do not also send `job_exit` or wake an
already active main turn. `/btw` remains a
separate read-only aside: its completion is visible in the UI, not main input.

Children use the main compaction algorithm on their own conversation projection,
including recent-message/tool-pair retention, exact archive and atomic handoff.
Successful child compaction emits `child_compacted` to the main agent under the
same ordering/delivery rules. Child-only compaction preserves child ID, running
turn, jobs and interactions; it never switches the main session or changes the
shared undo baseline. Main compaction preserves all live children. See
[compaction.md](compaction.md); chunked summaries are outside the requirements.

Before each model request, append an inspectable developer runtime-context
message containing live jobs/timers and changes, sampled at its committed event
cutoff. Stable coding instructions stay separate. This snapshot cannot revive
live work or establish a later job outcome. Delivered
notifications are ordinary historical messages; undelivered ones are transient.

Compaction creates a new conversation session inside the **same runtime**.
Keep jobs, timers, pending input, child contexts, and their IDs intact. Serialize
the context handoff with file commits; completions use the runtime's current
session ID. No durable job-routing or timer transfer is needed. See
[compaction.md](compaction.md).

Explicit main-session switching (`/new`, `/clear`, or loading another session)
and app exit cancel/join foreground work, children, shell/LSP process groups,
and timers; discard queued input and pending interactions. Show that queued
input is unsent and session-local. Normal shutdown records interruptions and
available bounded output. Loading history never revives work or handles.
Browsing a child view is not a session switch. Detaching tmux leaves the process
running; quitting TTC does not. Long-running research jobs belong in tmux.
After a crash, mark unfinished historical calls/turns interrupted without
relaunching them. Live handles from an earlier runtime return `not_found`.

## Session naming

After the first settled tool batch in a new main session (or completed response
without tools), make one extra
LLM request to name that session. Use the provider and model selected for
that boundary, with tool definitions omitted. Supply its first user message
(4 KiB) and current assistant reply (4 KiB), plus up to four tool names, with
any truncation marked.
Ask for a plain, descriptive title of three to six words. Cap output at 32
tokens; trim surrounding quotes, Markdown, and whitespace. Reject control
characters, line breaks, names outside three to six words, and names over
60 characters; do not truncate a word. Use one transport attempt with no automatic retry; time out after 15 seconds. A failed
or invalid response leaves the default name in place and records a visible
failure notice. Naming can finish while the main turn continues; a later failed
request does not revoke an already accepted title.

This request is session metadata work, not another conversation turn: it
does not add model-visible user/assistant messages or delay queued turns. Exact
inputs/replies remain inspectable. Record its claim and triggering turn ID in SQLite before sending the request, so
restarts never send it twice. A crash or timeout marks that
attempt failed. Commit a valid title and metadata event only if the session
still has its default name; a manual rename wins. Show the updated name in
the session list and header. Do not run this request for child agents or
compaction continuations, whose names follow [compaction.md](compaction.md).
If the first compaction starts
while naming is pending, wait for the bounded naming request to settle,
then freeze the root name used by all continuations.

## Data retention

Run cleanup on startup and every 24 hours. Expire a conversation lineage
(predecessors and continuations, including recorded child output) after 30 days
without committed history or user activity. The currently loaded lineage and
any pending filesystem operation are protected. In-memory timers/jobs cannot
keep an unloaded lineage alive because switching sessions cancels them.

Delete the lineage's conversation/tool records, request metadata, file snapshots,
attachments, and archives together. Keep all predecessor data referenced by a
retained continuation. Managed files are private and owned by one lineage;
avoid sharing blobs across lineages in v1. Use a deletion marker for retrying
interrupted cleanup. Credentials and workspace generation counters are not
conversation data. Resolve an absolute data root: `$XDG_DATA_HOME/scicode`, or
`~/.local/share/scicode` when `XDG_DATA_HOME` is unset or relative. Stay inside
that root and never follow symlinks when deleting managed files.

## Session history and undo

SQLite stores an immutable history tree with a selected cursor. Entries include
model-visible messages and UI/tool records; context projection includes only
messages for the main agent. Child records identify their actor and are visible
in the same chronological history, without entering the parent's model context.
Ctrl+X G opens the tree; arrows navigate, Enter selects a restorable branch, and
Esc closes it. Compaction predecessors are read-only.

Serialize all parent/child `edit`, `write`, and `patch` calls through one
workspace mutation queue. Each change captures before/after bytes and modes,
then advances one shared main-session file history. There is no independent
child undo. Shell changes and outside-workspace effects remain non-undoable.

`/undo` requires an idle main turn, stops remaining jobs/children, and clears
transient input/timers. It rewinds the latest user turn's history and every
recorded edit after its starting checkpoint, including child edits and later
async work. A failed/interrupted turn can also be undone after stopping work.
The shared admission/mutation gate serializes the starting checkpoint with
complete file commits; launch-turn attribution does not override commit order.
Child edits while the parent is idle extend that suffix; after a new user turn
starts, subsequent edits belong to the new chronological suffix. `/redo`
restores the recorded suffix without rerunning tools or reviving child handles.
Redo and branch selection enforce the same idle/stop boundary before restoration.
New input after undo creates a sibling branch and clears the implicit redo target.

Refuse restoration when affected files differ from expected states. Use a small
filesystem journal for partial writes and interrupted restores, without advancing
the history cursor prematurely. Loading a different session does not restore
files. If another TTC session has changed the workspace generation, set an
explicit undo boundary at the loaded history tip; only new work can be undone.
This avoids cross-session restoration. Older history remains browsable.

Retained complete turns remain undoable after compaction. A cut inside an active
turn establishes its earlier edits as baseline; only the retained suffix is
undoable. See [design.md](design.md) for mutation and persistence contracts.

## `@` attachments

Typing `@` in the composer opens a path picker for text files, directories,
and images. Selection inserts a visible attachment token; a literal `@`
remains text unless a path is selected. On submit, resolve and snapshot each
attachment so queued and steered messages keep the selected version. A text
file contributes its path and up to 32 KiB of contents. A directory contributes
its path and up to 500 sorted recursive paths; list symlinks but do not follow
them. Mark either limit explicitly; the agent can use `read` or `glob` for
more. An image contributes its path and bytes as multimodal user input.
Reject missing, unreadable, or unsupported files, and reject images when the
selected provider cannot accept them; never silently drop an attachment.
Persist attachment metadata and bytes when their message enters history.
Queued input and its snapshots remain transient until promoted; they do not
survive explicit session switches or restart.

## Minimal TUI and keys

Show the LLM conversation in the main pane and keep the input box anchored at
the bottom. Keep a one-line turn indicator fixed just above the input box,
outside the scrolling conversation. While a turn is active, show
`Working · <seconds>s`, updating whole elapsed wall seconds once per second;
show `Waiting for answer · <seconds>s` when a question suspends it. Keep it
fixed while streaming or scrolling; show `Retrying · <seconds>s` during
adapter backoff. On turn end, remove the indicator and append one status
message to conversation history, such as
`Turn completed · avg 42.3 tok/s`. Use `completed`, `interrupted`, or `failed`
to match the result. Compute the average across that turn as total
provider-reported output tokens divided by total wall seconds spent in
model requests, excluding tool, queue, and user-wait time. Show one decimal;
use `avg N/A` if usage or timing is unavailable. Persist the status as a TUI
history event for replay, outside the LLM's model context.

Make every conversation and internal request message inspectable, including
system instructions. Display system prompts as placeholders, never expanded
bodies in the main conversation. Clicking a placeholder opens its exact prompt
in the same reusable scrollable window used for messages and tool inspection.
Provide a keyboard path and a plain-terminal `/inspect <entry-id>` command.
Keep authentication and device-code login outside conversation history.

Show every tool call as a compact, renderable summary in conversation history,
including running and failed calls. Clicking its summary opens the shared
inspector with the call arguments, full result or error, and any retained
output or file diff. Diff rows elsewhere in the UI open the same inspector.
The inspector is one scrollable popup: mouse wheel, Up/Down, Page Up/Down,
and Ctrl+U/D scroll its content; Esc closes it. Keep a keyboard path to open
the focused summary or diff with Enter (Space in the history tree, where
Enter selects a branch). Page large details from retained storage instead of
loading them all into the TUI. Tool summaries and inspector details use portable Markdown through the shared
renderer, also used for session export and compaction archives. They are
presentation records, not extra model messages. `/export <path>` exports the
selected branch and its referenced assets at a committed event cut; see
[design.md](design.md) for the serialization and export contract.

A right sidebar has separate expandable lists for estimated context usage, live
commands/subagents, and scheduled timers. Click headers to collapse/expand; wheel
scrolls only the hovered list. Context is the latest parent coding request's
frozen model, estimated input breakdown and separately labeled output/safety
reserves, not reported usage. Refresh live metadata without copying captured
output or reading history. Show armed clicks beside their thumbnails. Below 100
columns, Ctrl+X S opens the sidebar overlay; Tab changes list focus, arrows/page
keys scroll, and Left/Right collapses/expands. Questions and history use overlays.
A concise workspace header shows cwd (the configured workspace), the nearest
containing Git root and current branch when available. Clip paths from the left,
with full metadata on click. Refresh optional Git metadata outside the UI loop.
Running calls show their current state, and live jobs/agents show concise titles;
subagent labels are required nonblank single lines, capped at 64 characters.
An image-click request uses the mouse and explicit confirmation. The composer shows selected `@` attachments before
submission.

Ctrl+U scrolls the conversation up by half a viewport; Ctrl+D only scrolls
it down, including with an empty composer. Ctrl+C exits and cancels work from
any view. A focused inspector handles Ctrl+U/D itself. In the composer,
Up/Down recalls human submissions, with the unfinished draft restored past
the newest entry; Alt+Up/Down focuses message rows for inspection. Menus/forms
use their own arrow navigation. Esc twice interrupts only with the composer
focused; Esc in a dialog first returns or dismisses that dialog.
Scrolling enters scroll mode and anchors the top visible message ID and text
offset while new text streams in or the layout changes. Reaching the bottom
with Ctrl+D does not resume auto-follow. The composer still accepts typing
and commands. Sending with Enter or Alt+Enter exits scroll mode and resumes
auto-follow. A slash command that submits a prompt does the same.
Ctrl+X F toggles a full-screen conversation view and preserves the scroll
anchor when used in scroll mode. Full screen hides the status box and composer
but keeps the turn indicator fixed at the bottom; typing reveals the composer
as a bottom overlay. Sending returns to the normal layout.
Ctrl+X L opens the session list; Enter loads the selected session. Mark
compaction predecessors read-only there and in their conversation view.
Compaction automatically switches the TUI to its new continuation.
`/clear` is an alias for `/new`: create and load a new empty main session.
Preserve the previous history in SQLite, but stop its active turn, jobs,
children, and timers. Clearing does not delete history or undo workspace edits.

Ctrl+X is the leader. Ctrl+X E opens `$VISUAL` or `$EDITOR` for the current
draft; saving returns text to the composer without sending it. Slash commands
and Ctrl+P expose actions when a terminal does not pass a shortcut. The first
Esc closes the focused modal first. In an image preview, it cancels only that
preview's live pending click; ordinary image dismissal emits no model message.
In the composer, Esc closes the sidebar overlay, otherwise leaves fullscreen,
otherwise returns a scrolled conversation to follow-tail. With no transient
state it arms interruption; a second consecutive Esc interrupts foreground work.
Any other key resets the Esc sequence.
Alt+Enter targets the displayed session, including a child session
when that child's view is open.

| Action                   | Key / command            |
| ------------------------ | ------------------------ |
| Send or queue            | Enter                    |
| Steer active turn        | Alt+Enter                |
| Newline                  | Shift+Enter or Ctrl+J    |
| External editor          | Ctrl+X E or `/editor`    |
| Command palette          | Ctrl+P                   |
| Scroll conversation      | Ctrl+U / Ctrl+D          |
| Interrupt turn           | Esc, Esc                 |
| Background all shells    | Ctrl+B                   |
| Background shell manager | `/background`            |
| Jobs                     | Ctrl+X J or `/jobs`      |
| Pending questions        | Ctrl+X ? or `/questions` |
| Timers                   | Ctrl+X T or `/timers`    |
| Prompt recall            | Up / Down               |
| Focus message row        | Alt+Up / Alt+Down        |
| New session              | Ctrl+X N or `/new`       |
| Clear to new session     | `/clear`                 |
| Session history tree     | Ctrl+X G                 |
| Full-screen history      | Ctrl+X F                 |
| Undo / redo              | `/undo` / `/redo`        |
| Session list and load    | Ctrl+X L or `/sessions`  |
| Model picker             | Ctrl+X M or `/models`    |
| Provider login           | `/login`                 |
| Compact context          | `/compact`               |
| Exit from any view       | Ctrl+C                   |
| Exit TUI                 | Ctrl+X Q or `/quit`      |

With tmux's default Ctrl+B prefix, press Ctrl+B twice to pass Ctrl+B to TTC,
or use the palette action to background foreground shells.
