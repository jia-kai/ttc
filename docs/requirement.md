# TTC requirements draft

- Target research coding in headless Linux SSH/tmux sessions, including containers.
  This is the complete intended target, not a completed-feature list; see
  [README](../README.md) for setup/current behavior.
- One process owns runtime/TUI. Kitty renders Markdown/math; anchor input while
  responses stream.
- Detailed contracts linked below are part of this target. Their references to
  implementation sources are not claims that every requirement is complete.

## Provider and context

- Keep messages/tools/streams/usage/cancellation provider-neutral. Start with
  OpenAI subscription; providers own catalogs/capabilities, image input and typed
  device-code login. Frontends render steps; never store secrets in history.
  Provider switches reconstruct local history, not foreign response IDs.
- Catalogs define models/variants/capacities/output limits/retention/headroom.
  Record turn-start/per-request selections; expose inspectable switching without
  changing active requests or children. [Model contracts](models.md) define catalog,
  startup, variants, Fast tiers, replay compatibility and metering details.
- Keep stable instructions/tool definitions. Temporarily forbid tools through a
  provider's hard no-tools mode or by omitting definitions, never artificial tool
  errors. Canonical LLM assets and runtime use are in [system_prompt.md](system_prompt.md).
- Retry transient pre-output failures until interrupted by default, without
  duplicating uncertain work. Preserve/fail partial streams; never execute incomplete
  calls, resume streams or restart unfinished requests. Require inspectable status
  and interruptible waits under [request retries](design.md#request-retries).
  Persist retry/final status, IDs, timing and usage.
- Keep native replay separate from visible text and reject incompatible reuse;
  [replay validation](design.md#provider-and-model-abstraction) and
  [model compatibility](models.md#switching-and-replay) define the contract.
- Compact automatically near capacity or with `/compact`. Create named main
  continuations and freeze predecessors while preserving inspectable source history
  and searchable archives. Visible input matches the summary/recent messages sent.
  Main and child cuts follow [compaction.md](compaction.md), including retention,
  single-pass summary, continuation naming, handoff and failures.

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
- Embed/discover `default-skills` without disk copies; project overrides user,
  then bundled. The LSP skill owns language/server setup; the backend owns protocol
  and cleanup. [LSP contracts](tools.md#lsp) define managed stdio and file sync.

## Scratch experiments

- Before inference, create shared sticky 1777 `/tmp/ttc` (any owner) and owned
  0700 `/tmp/ttc/{effective_numeric_uid}`. Reject symlinks/wrong ownership/modes;
  apply modes independently of umask, never repair unsafe existing paths.
- Supply its absolute runtime-context path; reverify/recreate before use after
  `/tmp` cleanup. Report unsafe/unavailable paths instead of supplying them.
- One-time scripts/probes use scratch cwd; requested project edits stay in the
  workspace. Scratch is ephemeral/outside SQLite and undo: no archives, attachment
  snapshots or retained output artifacts there.

## Async behavior

### Event ordering and main timeline

- Require one durable, monotonic semantic event order across actors, with causal
  attribution and human-checkpoint undo ownership. Transient streams/progress are
  not historical events; queued input enters history only on admission.
- Admission must freeze and acknowledge exactly the committed input delivered;
  failures consume nothing, later arrivals remain pending and copies never redeliver.
  File commits and human checkpoints must share a gate. The full sequence,
  transaction and lifecycle contract is [design ordering](design.md#event-ordering-and-main-timeline).

### Input and background work

- Parent/child requests may overlap without cycle limits. Require
  [tool batch ordering](tools.md#common-contracts); dependencies need later responses.
- **Enter:** idle starts a normal turn; busy steers active main work after
  response/tools settle, retaining the TTC turn. **Alt+Enter:** idle sends a normal
  turn; busy queues FIFO, visibly unsent and excluded from input until promotion.
  No native streaming/child steering.
- **Esc, Esc:** interrupt request/foreground tools, leaving independent jobs alive.
  **Ctrl+B:** promote foreground shells without canceling; a race with completion
  returns one final result. `/background` selects live shells; none is a no-op.
- **Shell completion:** persist bounded output/update UI. `wake_on_exit=true`
  (default) notifies at a boundary or starts idle work; false is UI-only. Never
  duplicate foreground results as completion notices.
- **Shell deadlines:** enforce the foreground deadline and optional background
  deadline in the [shell contract](tools.md#shell).
- **Question:** main only, one pending round; composer/jobs remain usable. Require
  explicit submission, preserved drafts and dismissal/redirection rather than
  implied answers. [question](tools.md#question) defines the dialog and results.
- **Wakeup:** deliver ahead of queued prompts; repeating firings coalesce without
  losing later arrivals. Require [admission/coalescing ordering](design.md#event-ordering-and-main-timeline)
  and [wakeup contracts](tools.md#wakeups). Exit/switch never restores scheduling;
  explicit [compaction-boundary loads](compaction.md#reload-recovery) may recover
  committed, undelivered firings into fresh notification events.
- **Image click:** return immediately and require explicit confirmation of source
  coordinates. [image_show](tools.md#image_show) defines actor ownership,
  cancellation and snapshot retention.
- Persist conversation/tool/file history, never execution. Require the
  [durable/live boundary](design.md#boundaries): compaction preserves live work;
  explicit session changes/exit cancel it, and loads never relaunch calls.
  Long work belongs in tmux, not a durable TTC scheduler.
- Supply inspectable [runtime snapshots](system_prompt.md#runtime-snapshots)
  separately from stable instructions. They report observed state, never revive
  work or establish future outcomes.

### Child agents

- Require isolated assignments with explicit persistence, bounded capacity,
  stable identity, frozen models and idle-only follow-ups. Children share serialized
  workspace edits, not parent input or independent undo; they cannot spawn children
  or ask the user questions. [subagent](tools.md#subagent) defines lifecycle,
  limits and results; [async messages](tools.md#async-messages-sent-to-the-model)
  defines exactly-once completion/compaction delivery.
- Child [compaction](compaction.md) must preserve live work without switching main
  history or undo. `/btw` must enforce read-only tools on frozen main context and
  keep answers/events outside parent input; see [aside design](design.md#read-only-side-questions).

## Session naming

- After the first settled main tool batch or tool-free final response, send one
  no-tools metadata request with that boundary's model: first user/current assistant
  text up to 4 KiB each and four tool names; mark truncation.
- Request a plain descriptive title using the canonical format and request limits
  in [prompt/naming.yaml](../prompt/naming.yaml). Trim quotes/Markdown/whitespace;
  reject controls, line breaks and titles outside those word/character bounds,
  never cutting a word.
  Invalid/failure leaves default name and an inspectable notice.
- Persist claim/trigger before inference so restart cannot resend; mark crash/
  timeout failed. Inputs/replies are inspectable but outside model history, do not
  delay queued turns, and may finish while main continues. Later failures do not
  revoke a title. Commit only while name remains default; manual rename wins.
- Show title in header/list. Do not name children or continuations. Compaction
  waits for bounded pending naming then freezes root name.

## Data retention

- Clean at startup/hourly; refresh loaded-session activity for other instances.
  Expire entire lineages after 30 inactive days; copies share retention.
- Delete history, tools, requests, snapshots, attachments and archives together;
  keep predecessor data required by retained continuations. Private managed files
  belong to one lineage, without cross-lineage blobs. Asset deletion is best
  effort without restart repair; credentials/preferences are not conversation data.
- Use absolute `$XDG_DATA_HOME/ttc`, falling back to `~/.local/share/ttc` when unset
  or relative. Deletion stays inside managed root and never follows symlinks.

## Session history and undo

- Require immutable SQLite history with selectable branches, inspectable child
  records outside main input and one serialized parent/child file-undo history.
  Undo must include async edits after the latest human checkpoint; redo restores
  snapshots, never tool execution. Restoration requires idle/stopped work and
  rejects file conflicts. Shell/outside-workspace effects are non-undoable.
- Session loading must copy balanced history without restoring files/live work;
  only new work is undoable. No crash repair or cross-instance workspace locking.
  The complete branch, restore, load and compaction-baseline contract is
  [serialized edits and shared undo](design.md#serialized-edits-and-shared-undo).

## `@` attachments

- `@` opens text/directory/image paths; selected tokens are visible, literal `@`
  remains text. Resolve/snapshot on submit so queued/steered input keeps that version.
- Text supplies path/up to 32 KiB; directories up to 500 sorted recursive paths,
  listing but not following symlinks. Mark limits; tools can page. Images supply
  path/bytes as multimodal input. Reject missing/unreadable/unsupported assets or
  provider-incompatible images; never silently drop them.
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
- Show compact highlighted running/failed tool summaries; click/focus opens
  parameters, result/error, retained output or diff. Page large details from storage.
  Shared portable Markdown serves UI/export/archives, adding no model messages.
  `/export <path>` writes the selected cut/assets.
- Windows: wheel/arrows/pages/Ctrl+U/D scroll, Esc closes, Enter opens focused
  rows/diffs (Space in history; Enter restores). Show borders/scroll indicators
  for windows/conversation; left-align assistant heading, indent content two.
- Sidebar independently collapses/scrolls context, jobs/children and timers. Display
  parent context separately from all-agent usage under [metering rules](models.md#usage-accounting).
  Refresh copied metadata without capture/history scans. Below 100 columns,
  Ctrl+X S opens overlay; Tab focuses,
  arrows/pages scroll and Left/Right collapse. Show session/cwd/Git root/branch;
  roll overflowing text horizontally, with full path inspection and Git refresh
  outside drawing. Live tools/agents have states/titles under the
  [child label contract](tools.md#subagent).
- Require cross-session prompt recall and highlighted bounded search under the
  [composer contract](design.md#composer-and-session-windows), restoring draft past
  newest. Alt+Up/Down focuses conversation; dialogs own arrows.
- Ctrl+U/D scroll half viewport. Ctrl+D never exits; at bottom it resumes follow.
  Scrolling anchors message/source offset through streams/resize. Reaching bottom
  alone does not follow. Enter/Alt+Enter/prompt-submitting commands follow again.
  Ctrl+C exits/cancels from any view; inspectors handle their own Ctrl+U/D.
- Require a frozen fullscreen copy view while runtime continues, with preserved
  reading anchor and live-view restoration. [Terminal rendering ownership](design.md#terminal-rendering-ownership)
  defines fullscreen layout, input and focus behavior; sending leaves fullscreen.
- Ctrl+X L lists dated/selectable sessions; Enter loads. Predecessors are read-only;
  compaction selects its continuation. New/clear preserve old history and edits but
  cancel work/transients. Every coding request uses current instructions and
  initial cwd/repository/branch metadata; no load acknowledgment popup.
- Ctrl+X E runs `$VISUAL`/`$EDITOR` on draft, returning without sending. Slash
  completion and Ctrl+P expose actions; palette inserts a command, Enter submits,
  cancellation preserves draft. `@` path completion must stay responsive.
- Esc exits focused editing/modal first; image Esc cancels only that pending click.
  In composer: close sidebar, leave fullscreen, then restore follow-tail. Otherwise
  arm interruption; next consecutive Esc interrupts foreground work. Other keys
  reset the sequence. Enter steers main only while busy; child inspectors stay read-only.
- Images use aspect-preserving bounded thumbnails and bordered pan/zoom previews
  with source coordinates/explicit confirmation. Math uses Kitty image placements
  with readable TeX fallbacks. See [tools](tools.md#image_show)/[MathJax](mathjax.md).

| Action                   | Key / command                 |
| ------------------------ | ----------------------------- |
| Send idle / steer busy   | Enter                         |
| Send idle / queue busy   | Alt+Enter                     |
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

- With tmux's default Ctrl+B prefix, press twice to pass it to TTC, or use
  `/background`/the palette.
