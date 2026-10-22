# Context compaction

Compaction replaces an actor's input with a developer context message containing
a short continuation hint and the generated Markdown summary verbatim, followed
by retained inputs and recent messages. Main creates a continuation and freezes
its predecessor; child compaction changes only child context. Jobs, timers,
queued input and interactions stay live; explicit session changes/exit cancel
them.

## Trigger and retention

- With capacity `C`, estimated coding input `I`, coding output allowance `O`,
  estimation margin `M`, summary output allowance `Q` and the appended summary
  instruction's estimated tokens `D`, compact when `I + O + M + Q + D >= C`. `D`
  includes instruction message framing. Nonempty manual focus adds a separate
  message immediately before the final summary instruction; both messages are
  counted in summary preflight. A handoff must satisfy:

  ```text
  fixed_input + compaction_context + retained_history + O + M + Q + D
      + next_turn_input_reserve < C
  recent_cycle_tail <= recent_tokens_max  (except unconsumed tool-result cycles)
  ```

  [Model budget metadata](models.md#budget-metadata) defines the fields and
  validation, including the distinction between context reserves and output
  caps.

- `/compact [focus]` requests main compaction. Admission assembles the next
  input and checks the budget above. The context estimator owns local counts and
  provider feedback. With a compatible measured request,
  `I = reported_input + local_next_input - local_measured_input`; otherwise use
  the full local estimate. Newly retained output is already in that difference;
  do not add reported output again. Missing feedback preserves an older
  compatible measurement. Compaction, history resets and model changes
  invalidate calibration. Native replay estimates model-visible occupancy, not
  encrypted transport JSON. Summary preflight uses the same actor estimator on
  the selected structured prefix plus its final instruction, preserving
  compatible coding feedback without updating its anchor. It reserves `Q + M`.
  The reserves are proactive slack: uncapped output, heuristic estimates,
  oversized committed input or model changes can still cause recoverable summary
  or handoff failures. Provider context-length rejection uses the bounded
  upstream retry budget; it does not trigger automatic compaction.
- `/force-compact [focus]` is an explicit idle main-actor command that bypasses
  only the estimated summary input-size check. It uses the same structured
  request and cache identity as `/compact`; automatic and child compaction keep
  their checks. It still validates messages, retention, summary output and final
  handoff capacity/headroom. Provider limits and bounded retries still apply.
  Upstream rejection alone is recoverable; invariant or persistence failures
  follow the failure policy below. Empty, read-only and unusable sessions cannot
  be forced. The saved compaction boundary records whether the request was
  forced.
- Preserve up to two latest admitted normal/queued prompts combined and up to
  two committed steers independently, chronologically, with exact
  text/attachments. Child tasks/asides count as normal/queued prompts.
  Unadmitted input stays live. Canonical history keeps each original input as an
  individual `llm.Message`, with exact `input_time_ms`, `input_source`, text and
  attachments. `retained_at_ms` records the compaction timestamp in UTC Unix
  milliseconds; no developer marker accompanies the input. Entry/source
  identities and undo checkpoints survive repeated compaction and loads. Model
  requests project each contiguous group of retained inputs into one developer
  format instruction and one structured user message, sorted by exact original
  timestamps ascending with stable ties. Items include explicit input types,
  original text/source, a relative timestamp formatted as
  `%.2f seconds before compaction`, and native attachment occurrence indices
  assigned after sorting. Epoch times remain canonical-only. Budget estimates
  include this exact projection; summarization preserves typed provenance. Live
  runtime notices do not survive ordinary loads. Only `RetainedInputs` guidance
  in [retained-inputs.md](../prompt/retained-inputs.md) is supplied to models;
  legacy marker decoding is isolated in startup migration.
- Retain complete assistant/tool cycles after the latest human input, not whole
  user/model turns. Target:

  ```text
  min(recent_tokens_max, max(recent_tokens_min, last_two_cycle_tokens))
  ```

  Keep the longest suffix within that target at a complete cycle boundary.
  Summarize oversized consumed cycles; never split tool pairs. Keep the newest
  call/result cycle outside summary input until a later assistant response
  consumes its results, including text and native binary results, even beyond
  the tail target. Overlapping pending calls move the cut back to the preceding
  complete cycle boundary. Human/runtime messages alone do not establish
  consumption. Compact preceding history first, then supply the fresh results
  unchanged after the summary. Unresolved calls block compaction. Subscription
  defaults: 4096–16000 estimated tokens, excluding independently retained
  inputs. The replacement must fit capacity/next-turn headroom. If it cannot
  fit, or no older model work or unretained human input can be summarized, leave
  the existing context unchanged and report the failure. Tool completion
  persistence bounds each batch's aggregate original payload below 20% of the
  producing model's context, independent of remaining conversation capacity.
  Bodies that exceed that intrinsic batch budget become model-facing retention
  notices; full originals remain in durable tool records. See
  [tool contracts](tools.md).

## Summary and history retrieval

- Freeze a committed cut; send its original structured actor prefix once with an
  appended developer instruction from
  [compaction.yaml](../prompt/compaction.yaml). Keep message roles, raw
  calls/results, compatible native replay and attachment references; use the
  normal retained-input projection, not a text transcript. Preserve the actor's
  model, system instructions, tool schemas and coding conversation/cache
  identity. Disable tools without removing their schemas. Append optional focus
  and the final summary instruction after the unchanged prefix, keeping it
  eligible for LLM input-cache reuse; actual hits depend on the provider.
  Attachments use normal resolution, including explicit unavailable-original
  notices. The temporary instruction and summary-request records never enter
  live coding input. Main-summary input/reply bodies remain inspectable in the
  frozen predecessor, not its writable continuation; small lifecycle metadata
  still follows the tail.
- The prompt requests optional handoff sections; their structure is guidance,
  not a validated schema.
- No chunking or regeneration. Reject empty/tool-calling summaries and summaries
  over 1 MiB. Estimated oversized input is refused unless explicitly forced.
  Output reserves follow [model metadata](models.md#budget-metadata);
  instructions live in [prompt/compaction.yaml](../prompt/compaction.yaml).
- Main, child and aside compaction use a cooperative 10-minute deadline for the
  whole operation, including naming, summary inference/retries and handoff.
  Earlier owning-operation deadlines and cancellation still apply. Synchronous
  storage I/O or lock waits can delay return after the deadline expires.
- Main, child and aside compaction write no eager transcript files. Durable
  SQLite history remains the source for inspection and on-demand
  [`conversation_export`](tools.md#conversation_export). Its actor-selected
  scope follows compaction and load-copy ancestry, deduplicates source entries
  and excludes sibling branches and unrelated actors. Main exports only main
  history; children export their own selected history across assignments; asides
  include their recorded frozen main prefix and their own history.
  `before_compaction` selects the latest successful actor cut, including its
  retained suffix, not later arrivals. Without a successful cut it fails. The
  tool contract specifies historical child/aside migration limits.
- Export files live in per-UID scratch, not durable archive storage. They can
  disappear with scratch cleanup and do not pin attachment cache contents. Stale
  archive links in old summaries do not prevent history-based exports.
- Drop opaque replay from replacement input; resupply full project instructions
  and current runtime context next request. Summaries are not live-job status.

## Handoff

- One shared engine selects retained history, freezes the actor's cut, requests
  the summary and constructs the canonical replacement for main, children and
  asides. Actor adapters supply only ownership-specific commits, scope and live
  state; they do not format or reinterpret the handoff.
- The first replacement message has type `compaction_context` and developer
  role. It is one Markdown message: a short hint to continue the active task
  using the summary below, followed by the generated summary verbatim. The
  summary supplies continuation instructions, without JSON quoting, an enclosing
  code fence or reference-data disclaimers. All actors budget the exact
  canonical message; main persists it unchanged and isolated actors keep it in
  their input. A prior compaction context counts as summarized work for later
  compaction, not as an assistant response consuming fresh tool results.
- Pause only the compacting actor; others may append committed tails. Main takes
  the routing gate, waits for file mutations and rechecks summary, retained
  input, committed tail, pending notices and fresh context together. Abort if
  they do not fit capacity/headroom.
- One SQLite transaction creates `{lineage-name}-cont-0`, then `-cont-1`,
  inserts summary/suffix/tail and freezes the predecessor. Preserve source/event
  identity; update the runtime session pointer before releasing the gate.
  Summary-first input is a projection, not global event order. Retained
  checkpoints stay undoable; pre-cut edits establish the continuation floor.
  Restoring just before a retained input uses its original checkpoint. Startup
  migration preserves legacy cursor identities and evidence by converting marker
  rows to nonvisible status entries; current replay does not decode markers.
- Child handoff preserves ID/read-only policy, leaving main session/file
  tip/undo untouched. Validate pending guidance and the next runtime-notice
  batch under the publication gate without consuming either. Commit one
  `child_compacted` on success with child/turn, estimated usage, not archive
  links or the summary body. Failure ends the child turn without a success
  event. `/btw` uses the same algorithm with events/answers outside main input.
- Refresh main estimated occupancy immediately; label previous reported usage
  separately. Summary inference counts once in run totals even if handoff fails.

## Failure policy

- Cancellation, transport interruption, timeout, rate limits, temporary server
  failure or exhausted disk leave prior context usable. Failed/interrupted turns
  pause automatic notification turns without consuming pending messages. An
  explicit prompt, successful `/compact`, `/force-compact` or `/load`, or
  session/model change resumes them; failure alone never starts another
  compaction attempt.
- Summary input/output size limits, replacement capacity/headroom failures and
  absence of older summarizable history are recoverable: keep the original
  context and live jobs/timers/interactions. A failed capacity check does not
  reject already admitted tool results. The actor may need smaller input or a
  larger-context model before it can continue.
- Other summary/invariant/persistence failures invalidate affected context.
  Persist main failure and block the live process even if that write fails;
  cancel jobs/timers/interactions. History remains inspectable/exportable. Start
  or load another usable session; reloading does not clear failure. Failed
  children close without invalidating main.
- Failed handoff never switches/freezes main history. Child failure/cancellation
  still captures eager immutable diagnostic transcripts; these are not
  compaction archives. Startup leaves execution records untouched; manual
  loading copies balanced context without work repair or restored jobs, timers
  or pending clicks.

## Reload recovery

- Reload reads durable history and needs no compaction archive files. Historical
  records, including assistant-summary roles/content, saved requests and archive
  files remain unchanged; loading does not reclassify or wrap them. Schema 11
  fences the expanded message vocabulary without converting existing records.
- `/load ID` and `--session ID` copy writable history at its last complete tool
  exchange. If selected history ends at main compaction or a handoff before the
  next coding request, recover committed, undelivered child completions and
  runtime notifications. Recovered snapshots keep this boundary until coding
  resumes. Ordinary loads and read-only/fatal contexts do not recover messages.
- Delivery is automatic; if context is still full, a new compaction precedes the
  coding request. `/compact [focus]` while idle also preserves recovered
  messages. Partial replies remain inspectable, but are never reused as
  successful summaries.
- Recovery creates fresh notification events in the loaded session in commit
  order, with `recovered_from_event_seq` identifying historical notifications.
  History returns pending-event data; session applies the same model-facing
  projection as live notifications, without restoring live child handles. It
  does not acknowledge or modify the source session, include sibling branches,
  replay delivered events, or duplicate child answers carried by retained main
  tool results. Repeated undelivered timer firings coalesce to the latest
  firing. Recovered messages survive another load before delivery.
- Delivered means admitted to a coding request, even if that request later fails
  or is interrupted; recovery does not replay those messages.
- Loading does not restore files, jobs, children, timers or pending
  interactions. Memory-only queued human prompts and unadmitted steering cannot
  be recovered from a killed process. Only already committed notification bodies
  are recovered.
