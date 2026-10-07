# Context compaction

Compaction replaces an actor's input with a Markdown summary and recent messages.
Main creates a continuation and freezes its predecessor; child compaction changes
only child context. Jobs, timers, queued input and interactions stay live;
explicit session changes/exit cancel them.

## Trigger and retention

- With capacity `C`, coding output allowance `O`, estimation margin `M` and
  estimated input `I`, compact when `I + O + M >= C`. A handoff must satisfy:

  ```text
  fixed_input + summary_and_archive_links + retained_history + O + M
      + next_turn_input_reserve < C
  recent_cycle_tail <= recent_tokens_max  (except unread binary-tool cycle)
  ```

  [Model budget metadata](models.md#budget-metadata) defines the fields and
  validation, including the distinction between context reserves and output caps.
- `/compact [focus]` requests main compaction. Before main/child admission,
  estimate instructions, tools, attachments, history and output allowance against
  capacity; compact before overflow. Native replay estimates model-visible
  occupancy, not encrypted transport JSON; reported usage is separate. Provider
  context-length rejection uses the bounded upstream retry budget; it does not
  trigger automatic compaction.
- Preserve up to two latest admitted normal/queued prompts combined and up to two
  committed steers independently, chronologically, with exact text/attachments.
  Child tasks/asides count as normal/queued prompts. Unadmitted input stays live.
  Each copied input gets a separate marker: source, original commit time,
  compaction time and age. Repeated compaction preserves identity/original time.
  Markers survive manual loads; live runtime notices do not. Canonical instructions:
  [prompt/retained-input.md](../prompt/retained-input.md).
- Retain complete assistant/tool cycles after the latest human input, not whole
  user/model turns. Target:

  ```text
  min(recent_tokens_max, max(recent_tokens_min, last_two_cycle_tokens))
  ```

  Keep the longest suffix within that target at a complete cycle boundary.
  Summarize oversized cycles; never split tool pairs. Exception: retain the newest
  cycle with binary tool results until a later assistant response consumes them,
  even beyond the tail target. Fail if that cycle cannot fit capacity/headroom;
  human/runtime messages alone do not establish consumption. Unresolved calls block
  compaction. Subscription defaults: 4096–16000 estimated tokens, excluding
  independently retained inputs. The replacement must fit capacity/next-turn
  headroom. Fail if no older model work or unretained human input can be summarized.

## Summary and archives

- Freeze/archive a cut; summarize its canonical actor prefix once, without tools,
  using the actor's model and summary output allowance. The canonical
  [summary instructions](../prompt/compaction.yaml) define handoff content.
  Include raw arguments/results, not UI activity or expanded tool views. Binary
  attachments enter summary inference as metadata only, not original bytes;
  summaries can preserve prior observations, not inspect unseen contents.
- No chunking, regeneration or mandatory headings. Reject empty/tool-calling
  summaries, oversized summary requests and summaries over 1 MiB. Output reserves
  follow [model metadata](models.md#budget-metadata); input/link templates live in
  [prompt/compaction.yaml](../prompt/compaction.yaml).
- Main, child and aside compaction use a cooperative 10-minute deadline for the
  whole operation, including naming, archives, summary inference/retries and
  handoff.
  Earlier owning-operation deadlines and cancellation still apply. Synchronous
  storage I/O or lock waits can delay return after the deadline expires.
- Private immutable archives belong to the main lineage:

  ```text
  <data-root>/lineages/<lineage-id>/compactions/<content-hash>.md
  <data-root>/lineages/<lineage-id>/compactions/<content-hash>.md.jsonl
  ```

  Main Markdown matches dense `/export`, omitting system bodies/duplicated request
  payloads. JSONL preserves exact envelopes, tool records, instructions, actors,
  replay and attachments. Child archives contain selected child messages; `/btw`
  includes its frozen main prefix. Source history remains inspectable. Include
  retained suffixes; later arrivals belong to another archive.
- Append verified absolute archive paths and grep/read guidance to the summary.
  Drop opaque replay from replacement input; resupply full project instructions
  and current runtime context next request. Summaries are not live-job status.

## Handoff

- Pause only the compacting actor; others may append committed tails. Main takes
  the routing gate, waits for file mutations and rechecks summary, retained input,
  committed tail, pending notices and fresh context together. Abort if they do
  not fit capacity/headroom.
- One SQLite transaction creates `{lineage-name}-cont-0`, then `-cont-1`, inserts
  summary/suffix/tail and freezes the predecessor. Preserve source/event identity;
  update the runtime session pointer before releasing the gate. Summary-first
  input is a projection, not global event order. Retained checkpoints stay undoable;
  pre-cut edits establish the continuation floor.
- Child handoff preserves ID/read-only policy, leaving main session/file tip/undo
  untouched. Commit one `child_compacted` on success with child/turn, archive links
  and estimated usage, not the summary body. Failure ends the child turn without
  a success event. `/btw` uses the same algorithm with events/answers outside main
  input.
- Refresh main estimated occupancy immediately; label previous reported usage
  separately. Summary inference counts once in run totals even if handoff fails.

## Failure policy

- Cancellation, transport interruption, timeout, rate limits, temporary server
  failure or exhausted disk leave prior context usable. Failed/interrupted turns
  pause automatic notification turns without consuming pending messages. An
  explicit prompt, successful `/compact` or `/load`, or session/model change
  resumes them; failure alone never starts another compaction attempt.
- Other summary/budget/invariant/persistence failures invalidate affected context.
  Persist main failure and block the live process even if that write fails;
  cancel jobs/timers/interactions. History remains inspectable/exportable. Start
  or load another usable session; reloading does not clear failure. Failed children
  close without invalidating main.
- Failed handoff never switches/freezes main history. Unused archives are
  disposable with their lineage. Startup leaves execution records untouched;
  manual loading copies balanced context without work repair or restored jobs,
  timers or pending clicks.

## Reload recovery

- `/load ID` and `--session ID` copy writable history at its last complete tool
  exchange. If selected history ends at main compaction or a handoff before the
  next coding request, recover committed, undelivered child completions and
  runtime notifications. Recovered snapshots keep this boundary until coding
  resumes. Ordinary loads and read-only/fatal contexts do not recover messages.
- Delivery is automatic; if context is still full, a new compaction precedes the
  coding request. `/compact [focus]` while idle also preserves recovered messages.
  Partial replies remain inspectable, but are never reused as successful summaries.
- Recovery creates fresh notification events in the loaded session in commit
  order, with `recovered_from_event_seq` identifying historical notifications.
  It does not acknowledge or modify the source session, include sibling
  branches, replay delivered events, or duplicate child answers carried by
  retained main tool results. Repeated undelivered timer firings coalesce to the
  latest firing. Recovered messages survive another load before delivery.
- Delivered means admitted to a coding request, even if that request later fails
  or is interrupted; recovery does not replay those messages.
- Loading does not restore files, jobs, children, timers or pending interactions.
  Memory-only queued human prompts and unadmitted steering cannot be recovered
  from a killed process. Only already committed notification bodies are recovered.
