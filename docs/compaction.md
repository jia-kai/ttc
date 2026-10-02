# Context compaction

Compaction replaces one actor's input with a Markdown summary and recent messages.
Main compaction creates a continuation and freezes its predecessor; child
compaction changes only its context. The live runtime keeps jobs, timers, queued
input and interactions. Explicit session changes/exit cancel them.

## Trigger and retention

- `/compact [focus]` requests a main handoff. Before main/child admission, estimate
  instructions, tools, attachments, history and output allowance against model
  capacity. Compact before oversized admission. Native replay estimates count
  model-visible occupancy, not encrypted transport JSON; reported usage is separate.
  Provider context-length rejection fails directly, without compaction/retry.
- Retain complete recent user turns up to `recent_tokens_target`. If the active
  turn alone exceeds it, retain its initiating user/task message and last two
  assistant messages with complete tool results. Cut only after a completed cycle;
  never split pairs or discard unresolved calls. Mandatory retention may exceed
  its target, but the next request must fit capacity and next-turn headroom.
  Without a safe prefix, fail explicitly.

## Summary and archives

- Freeze/archive a cut and summarize its canonical actor prefix once, without
  tools, using that actor's model and summary output allowance. Preserve goals,
  constraints, decisions, results, open work and lookup details. Raw arguments/
  results enter the summary request; UI activity/expanded tool views do not.
- No chunks, regeneration or mandatory heading schema. Reject empty summaries,
  tool calls and over-budget input/output. Canonical summary instructions and
  templates live in [prompt/compaction.yaml](../prompt/compaction.yaml).
- Immutable private files belong to the main lineage:

```text
<data-root>/lineages/<lineage-id>/compactions/<content-hash>.md
<data-root>/lineages/<lineage-id>/compactions/<content-hash>.md.jsonl
```

- Main Markdown matches dense `/export`, omitting system bodies and duplicated
  request payloads. JSONL preserves original envelopes, exact tool records and
  instructions. Child archives hold selected child messages; `/btw` includes its
  frozen main prefix. Preserve actor, replay and attachment data in JSONL.
  Shared source history remains inspectable. Archives include retained suffixes;
  later arrivals belong to another archive.
- Append verified absolute Markdown/JSONL paths and grep/read guidance to the
  summary. Drop opaque replay from the replacement context. Resupply full project
  instructions and current runtime context at the next request; summaries do not
  establish whether jobs still run.

## Handoff

- Only the compacting actor pauses requests; others may append committed tails.
  Main handoff takes the routing gate, waits for file mutations and rechecks summary,
  retained messages, committed tail, pending notices and fresh context together.
  Abort if the assembled input does not fit.
- One SQLite transaction creates `{lineage-name}-cont-0`, then `-cont-1`, inserts
  summary/suffix/tail and freezes the predecessor. Copies retain source/event
  identities. Update the runtime session pointer before releasing the gate.
  Summary-first input is a projection, not global event order. Retained checkpoints
  stay undoable; pre-cut edits establish the continuation's floor.
- Child handoff preserves ID/read-only policy and changes no main session/file tip/
  undo checkpoint. Success commits one `child_compacted` with child/turn, archive
  references and estimated usage, without the summary body. Failed handoffs publish
  no success event and end that child turn. `/btw` uses the same algorithm but keeps
  events/answers outside main input.
- Main handoff refreshes estimated occupancy immediately. Previous reported usage
  remains separately labeled. Summary inference counts once in run totals even
  when the later handoff fails.

## Failure policy

- Cancellation, transport interruption, timeout, rate limits, temporary server
  failure or exhausted disk leave prior context usable. A later request may try
  after conditions change; there is no automatic recovery attempt.
- Other summary/budget/invariant/persistence failures invalidate the affected
  context. Persist main failure and block the live process even if that write
  fails; cancel jobs/timers/interactions. History stays inspectable/exportable.
  Start or load another usable session; reloading never clears failure. Close a
  failed child without invalidating main context.
- Failed handoff never switches/freezes main history. Unused archives are disposable
  with their lineage. Restart interrupts unfinished work and never revives jobs,
  timers or pending clicks.
