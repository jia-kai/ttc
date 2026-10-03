# TTC requirements draft

TTC targets research coding in headless SSH/tmux sessions on Linux, including
containers. This document defines the complete target; [README](../README.md)
describes setup/current behavior. One process owns runtime and TUI. Kitty renders
Markdown/math; input remains anchored while responses stream.

## Provider and context

- Keep messages, tools, streams, usage and cancellation provider-neutral. OpenAI
  subscription is initial; providers own catalogs/capabilities, image input and
  typed device-code login. Frontends render steps; secrets never enter history.
  Switching providers reconstructs local history, not foreign response IDs.
- Catalogs define models, variants, capacities, output limits, retention and
  headroom. Store turn-start and per-request selections. Switch after the current
  tool batch and append an inspectable message; active requests/children keep their
  selection. Advertised Fast choices share base model/reasoning with an explicit tier.
- Keep stable instructions/tool definitions. Temporarily forbid tools through a
  provider's hard no-tools mode or by omitting definitions, never artificial tool
  errors. Canonical LLM assets and runtime use are in [system_prompt.md](system_prompt.md).
- Retry transient transport, rate-limit and server errors until interruption by
  default, using the same model. Honor numeric/date Retry-After capped at 30 seconds;
  otherwise bounded exponential jitter. Show clickable request-linked notices
  before waits; keep them outside model input. Esc, Esc interrupts waits.
- Retry only before committed output/calls. Keep partial streams and fail; never
  execute incomplete calls, resume streams or resend uncertain work after restart.
  Persist retry/final status, IDs, timing and usage. Retain compatible native replay
  separately from visible text; never reuse across incompatible models/branches/
  compaction or replay an executed tool.
- Compact automatically near capacity or with `/compact`. Main continuation names
  use `{original_name}-cont-0`, `-cont-1`, etc.; predecessor becomes read-only.
  Visible input starts with summary/recent messages exactly as sent. Preserve full
  source history and searchable branch archives. Child cuts share the single-pass
  algorithm without switching main history. Oversized summary input fails. See
  [compaction.md](compaction.md) for retention, handoff and failures.

## LLM-facing tools

| Tool              | Purpose                                              |
| ----------------- | ---------------------------------------------------- |
| `read`            | Read a file/list one directory with paging           |
| `glob`, `grep`    | Bounded path/content search                          |
| `web_fetch`       | HTTP(S) readable text, source and truncation         |
| `web_search`      | Bounded source titles, URLs and snippets             |
| `shell`           | Foreground/background command in chosen cwd          |
| `edit`            | Exact replacement, unique unless `replace_all`       |
| `write`           | Create/overwrite from complete contents              |
| `patch`           | Structured add/edit/move/delete                      |
| `image_show`      | Inline image/dimensions, optional confirmed point    |
| `question`        | Single-choice/free-text question round               |
| `skill`           | Load named project/user/embedded instructions        |
| `lsp_query`       | Managed stdio definitions/references/hover/symbols   |
| `subagent`        | Isolated coding assignment or idle follow-up         |
| `job_list`        | Live command/child IDs, owners and states            |
| `job_read`        | Retained output/status/result                        |
| `job_stop`        | Stop process group or close child                    |
| `wakeup_schedule` | One-shot/repeating runtime reminder                  |
| `wakeup_list`     | Reminders, next firings and last results             |
| `wakeup_cancel`   | Cancel by ID/name                                    |

- [tools.md](tools.md) defines exact inputs/results; [prompt/tools.yaml](../prompt/tools.yaml)
  is canonical model guidance. Tool permissions belong to the host/container;
  TTC asks for no tool approvals.
- Embed `default-skills`; discover without disk copies. Project overrides user,
  then bundled. The LSP skill supplies language/server setup; the backend owns
  protocol correctness/cleanup. Start with `shell(background=true, protocol="lsp")`:
  reserve stdin/stdout for JSON-RPC, disable PTY, retain stderr, initialize and sync
  files. `job_read` never consumes protocol output.

## Scratch experiments

- Before inference, create `/tmp/ttc` as shared sticky 1777 (possibly another
  owner's) and `/tmp/ttc/{effective_numeric_uid}` as owned 0700. Reject symlinks,
  wrong ownership/permissions; apply modes independently of umask without repairing
  unsafe existing paths.
- Supply its absolute runtime-context path. Reverify/recreate before use after
  `/tmp` cleanup; report unsafe/unavailable paths instead of supplying them.
- One-time scripts/probes use scratch cwd; requested project edits stay in the
  workspace. Scratch is ephemeral/outside SQLite and undo: no archives, attachment
  snapshots or retained output artifacts there.

## Async behavior

### Event ordering and main timeline

- One serialized writer commits all actors' semantic events with durable monotonic
  `event_seq`, actor, causal request/call, chronological main turn and latest human
  `undo_owner_turn_id`. Never reuse sequences after restart/branching/cleanup;
  gaps are allowed. Timestamps are display metadata; workers choose no history position.
- Queued input remains transient until user/steer admission. Provider-ordered
  intents precede dispatch; each gets one terminal result. Results commit by arrival,
  while actor input reconstructs call order. Failed/interrupted streams execute no
  calls; streaming/progress are transient views, not durable events.
- Human admission/checkpoint and complete file apply/record share a mutation gate.
  Commit order decides undo ownership; idle writes extend the latest human suffix,
  while notification turns create no checkpoint. Launch IDs preserve provenance.
  Shell/external effects are outside serialized file undo.
- Freeze each request's committed cutoff; include eligible notices in sequence
  order and atomically acknowledge their IDs. Failed admission consumes nothing.
  Delivery means persisted inclusion through a result/notice, not a saved result
  alone or successful inference. Later arrivals stay pending. Idle wakes use the
  same gate, never another concurrent main turn.
- Copies preserve source/delivery identity and never notify again. Summary-first
  actor input differs from chronological lineage order; record handoff once.
  Closing rejects admission, joins work, records terminal states and discards
  undelivered notices before activation. See [design ordering](design.md#event-ordering-and-main-timeline).

### Input and background work

- Parent/child requests may overlap without cycle limits. Run read-only calls
  together, then ordered mutations/controls; shells/children overlap both. Ordering
  is best effort; dependent operations need later responses.
- **Enter:** idle starts a turn; busy queues FIFO, visibly unsent and excluded from
  input until promotion. **Alt+Enter:** steer active main work after response/tools
  settle, retaining the TTC turn. No native streaming/child steering.
- **Esc, Esc:** interrupt request/foreground tools, leaving independent jobs alive.
  **Ctrl+B:** promote foreground shells without canceling; a race with completion
  returns one final result. `/background` selects live shells; none is a no-op.
- **Shell completion:** persist bounded output and update UI. Default
  `wake_on_exit=true` notifies at a boundary or starts idle work; false is UI-only.
  Foreground results never duplicate completion notices.
- **Question:** main only, one pending round; children finish useful work, report
  material information gaps to main and stop. Keep composer/jobs usable. Tabs use Left/Right,
  final Submit sends all answers. Choose exactly one option or Other/free text;
  Up/Down focuses, Enter selects/advances, Space selects without advancing.
  Recommendation focuses without selection. Preserve drafts across tabs/types;
  Esc leaves editing then dismisses without answering. Only explicit `/questions`
  reopening clears dismissal. The next normal message returns `{dismissed:true}`
  and redirects main at its settled request boundary; local commands leave the
  round pending. Never restore pending forms on restart.
- **Wakeup:** deliver ahead of queued prompts at a boundary or while idle. Coalesce
  repeats to one outstanding latest immutable firing at/before cutoff, with its
  cumulative count/sequence. Ack only that firing; later arrivals remain pending.
  No missed-wakeup replay after exit/switch.
- **Image click:** `image_show(request_click=true)` returns immediately; allow one
  pending interaction per actor. Clicking inside selects source coordinates; OK
  explicitly confirms, Esc cancels. Retain exact displayed snapshot, not pending state.
- Persist conversation/tool/file history; jobs, timers, handles, queues, steers,
  questions, clicks and capture rings stay in memory. IDs in history are not live.
  Compaction preserves runtime/IDs; explicit new/clear/load/exit cancels and joins
  work, drops queues/interactions, and records interrupted output. Child inspection
  is not switching. tmux detach keeps TTC alive; long work belongs in tmux.
  Crash recovery never relaunches calls; old handles return `not_found`.
- Append inspectable developer runtime context only on committed changes, with
  jobs/timers/transitions. Activation/compaction force it. Stable instructions are
  separate; snapshots cannot revive work or establish future outcomes. Delivered
  notices become history; pending ones remain transient.

### Child agents

- Each child has stable ID and fresh turn/job IDs per assignment. Require explicit
  `persistent` every time: false closes context and joins owned work on completion;
  true retains successful idle context. Failure/cancellation always close. Retain
  at most four coding contexts, including idle ones; closing frees capacity.
- Only main assigns idle follow-ups; running children reject with wait-for-finish
  guidance and accept no steering/queues. Final completion waits for foreground
  tools/interactions. Children cannot spawn children; inspectors are read-only.
- Model identity freezes; optional supported reasoning `variant` inherits parent
  at creation or retains child choice when omitted on follow-up. Changes are idle-only.
- Commit terminal state and one `child_turn_finished` before accepting follow-up.
  Include status, child/turn/job, up to 8 KiB UTF-8 final answer with explicit
  truncation and exact immutable reply/error reference, never the full transcript.
  Foreground result carries/acknowledges finish at admission; background queues
  one notice. No extra `job_exit` or redundant wake for active main work.
- Share [compaction](compaction.md) on actor input. Successful cut emits one
  `child_compacted`, preserving child/turn/jobs/interactions without changing main
  session/undo floor. Main cuts preserve children. Chunked summaries are excluded.
  `/btw` shares frozen context with enforced read-only tools; its answer/events are
  UI-only, outside main input.

## Session naming

- After the first settled main tool batch, or final response without tools, send
  one no-tools metadata request using that boundary's model. Supply first user
  and current assistant text up to 4 KiB each plus four tool names; mark truncation.
- Request a plain descriptive 3–6-word title, 32-token output budget, one transport
  attempt and 15-second deadline. Trim quotes/Markdown/whitespace; reject controls,
  line breaks, invalid word counts or more than 60 characters, never cutting a word.
  Invalid/failure leaves default name and an inspectable notice.
- Persist claim/trigger before inference so restart cannot resend; mark crash/
  timeout failed. Inputs/replies are inspectable but outside model history, do not
  delay queued turns, and may finish while main continues. Later failures do not
  revoke a title. Commit only while name remains default; manual rename wins.
- Show title in header/list. Do not name children or continuations. Compaction
  waits for bounded pending naming then freezes root name. Canonical text/limits
  live in [prompt/naming.yaml](../prompt/naming.yaml).

## Data retention

- Clean at startup/every 24 hours. Expire entire lineages after 30 days without
  history/user activity, preserving current lineage and pending filesystem work.
  Unloaded lineages have no live jobs/timers to prolong them.
- Delete history, tools, requests, snapshots, attachments and archives together;
  keep predecessor data required by retained continuations. Private managed files
  belong to one lineage, without cross-lineage blobs. Retry interrupted cleanup
  through a deletion marker; credentials/workspace generations are not conversation data.
- Use absolute `$XDG_DATA_HOME/ttc`, falling back to `~/.local/share/ttc` when unset
  or relative. Deletion stays inside managed root and never follows symlinks.

## Session history and undo

- SQLite has immutable entries and a selected tree cursor. Main input includes
  main-visible messages; child records stay inspectable outside its context.
  Ctrl+X G shows linear human inputs with fork connectors; arrows navigate,
  Enter restores before input, Space inspects and Esc closes. Predecessors are read-only.
- Serialize parent/child edit/write/patch through one mutation queue, capturing
  before/after bytes/modes and advancing shared file history. No child undo;
  shell and outside-workspace changes are non-undoable.
- Undo/redo/branch selection require idle main work and stop/join jobs/children,
  clearing transients. Undo reverses everything after the latest human checkpoint,
  including child/trailing async writes and failed/interrupted turns. The gate
  orders checkpoint versus complete commits, not launch attribution. Redo restores
  saved bytes without tool execution/live handles. New input branches/clears redo.
- Reject restoration conflicts. Journal partial writes/restores before advancing
  cursors. Loading another session never restores files; changed workspace generation
  establishes a tip boundary so only new work is undoable, older history inspectable.
- Compaction keeps retained checkpoints undoable; summarized edits establish
  the baseline. See [design](design.md#serialized-edits-and-shared-undo).

## `@` attachments

- `@` opens text/directory/image paths; selected tokens are visible, literal `@`
  remains text. Resolve/snapshot on submit so queued/steered input keeps that version.
- Text contributes path/up to 32 KiB; directories contribute up to 500 sorted
  recursive paths, listing but not following symlinks. Mark limits; tools can page.
  Images contribute path/bytes as multimodal input. Reject missing/unreadable/
  unsupported assets or provider-incompatible images; never silently drop them.
- Persist snapshots when admitted. Queues/attachments remain transient until then
  and never survive session changes/restart.

## Minimal TUI and keys

- Anchor composer and one working indicator above it: `Working`, `Waiting for
  answer` or `Retrying · <seconds>s`, updating whole wall seconds. Queued/steered
  prompts sit beneath, one clipped line each. Human input has distinct background;
  system text distinct color. Input cursor blinks.
- Turn end appends `Turn complete · 2s · avg 42.3 tok/s` with readable elapsed time.
  Display complete/interrupted/failed, retaining canonical successful `completed`.
  Average reported output over model-request wall time only, excluding tools/queue/
  waits; show one decimal or N/A. Persist outside model context.
- Inspect every conversation/internal message. System placeholders open exact
  prompts in the shared bordered scrollable window, not expanded main rows.
  Auth/login stays outside history. Plain mode has `/inspect <entry-id>`.
- Tool calls show compact highlighted running/failed summaries. Click/focus opens
  parameters, result/error, retained output or file diff. Page large details from
  storage, not full TUI loads. Shared portable Markdown serves UI/export/archives;
  these views add no model messages. `/export <path>` writes the selected cut/assets.
- Window scroll: wheel, arrows, pages, Ctrl+U/D; Esc closes. Enter opens focused
  rows/diffs, Space in history because Enter restores. Show window/conversation
  scroll indicators and borders; assistant heading aligns left, content indents two.
- Sidebar independently collapses/scrolls context, jobs/children and timers. Display
  latest parent frozen model/input breakdown/reserves with estimates labeled;
  reported usage and all-agent totals stay separate. Totals survive compaction and
  session changes; only restart or `/new` clears them. Refresh copied metadata without
  capture/history scans. Below 100 columns, Ctrl+X S opens overlay; Tab focuses,
  arrows/pages scroll and Left/Right collapse. Show session/cwd/Git root/branch;
  roll overflowing text horizontally, with full path inspection and Git refresh
  outside drawing. Live tools/agents have states/titles; new child labels are
  1–4 words, ≤64 single-line characters.
- Up/Down recalls human input across sessions/restarts and restores draft past
  newest. Ctrl-R searches newest-first bounded prompt history; Enter fills input,
  Esc cancels. Alt+Up/Down focuses conversation; dialogs own their arrows.
- Ctrl+U/D scroll half viewport. Ctrl+D never exits; at bottom it resumes follow.
  Scrolling anchors message/source offset through streams/resize. Reaching bottom
  alone does not follow. Enter/Alt+Enter/prompt-submitting commands follow again.
  Ctrl+C exits/cancels from any view; inspectors handle their own Ctrl+U/D.
- Fullscreen Ctrl+X F preserves reading anchor, uses all columns, hides sidebar/
  scrollbar/composer, keeps bottom indicator and reveals typing overlay. Freeze
  display/automatic dialogs while runtime continues; disable mouse/clicks for copy.
  Keyboard scroll/resize works; exit restores live view/mouse; sending leaves fullscreen.
- Ctrl+X L lists dated/selectable sessions; Enter loads. Predecessors are read-only;
  compaction selects its continuation. New/clear preserve old history and edits but
  cancel work/transients. Refresh writable instructions at least one hour old for
  both loaded views and new requests; no load acknowledgment popup.
- Ctrl+X E runs `$VISUAL`/`$EDITOR` on draft, returning without sending. Slash
  completion and Ctrl+P expose actions; palette inserts a command, Enter submits,
  cancellation preserves draft. `@` path completion must stay responsive.
- Esc exits focused editing/modal first; image Esc cancels only that pending click.
  In composer: close sidebar, leave fullscreen, then restore follow-tail. Otherwise
  arm interruption; next consecutive Esc interrupts foreground work. Other keys
  reset the sequence. Alt+Enter steers main only; child inspectors stay read-only.
- Images use aspect-preserving bounded thumbnails and bordered pan/zoom previews
  with source coordinates/explicit confirmation. Math uses Kitty image placements
  with readable TeX fallbacks. See [tools](tools.md#image_show)/[MathJax](mathjax.md).

| Action                   | Key / command                 |
| ------------------------ | ----------------------------- |
| Send or queue            | Enter                         |
| Steer active turn        | Alt+Enter                     |
| Newline                  | Shift+Enter or Ctrl+J         |
| External editor          | Ctrl+X E or `/editor`         |
| Command palette          | Ctrl+P                        |
| Scroll conversation      | Ctrl+U / Ctrl+D               |
| Interrupt turn           | Esc, Esc                      |
| Background all shells    | Ctrl+B                        |
| Background shell manager | `/background`                 |
| Jobs                     | Ctrl+X J or `/jobs`           |
| Pending questions        | Ctrl+X ? or `/questions`      |
| Timers                   | Ctrl+X T or `/timers`         |
| Prompt recall/search     | Up / Down / Ctrl+R            |
| Focus message row        | Alt+Up / Alt+Down             |
| New session              | Ctrl+X N or `/new`            |
| Clear to new session     | `/clear`                      |
| Session history tree     | Ctrl+X G or `/history`        |
| Fullscreen history       | Ctrl+X F                      |
| Sidebar overlay          | Ctrl+X S                      |
| Undo / redo              | `/undo` / `/redo`             |
| Session list/load        | Ctrl+X L or `/sessions`       |
| Model picker             | Ctrl+X M or `/model`          |
| Provider login           | `/login`                      |
| Compact context          | `/compact`                    |
| Exit                     | Ctrl+C or Ctrl+X Q or `/quit` |

With tmux's default Ctrl+B prefix, press twice to pass it to TTC, or use
`/background`/the palette.
