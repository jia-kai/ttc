# Context compaction

Compaction replaces one actor's model context with a Markdown handoff and recent
messages. Main compaction creates a continuation session and makes its predecessor
read-only. Child compaction replaces only that child's input. Live jobs, timers,
queued input and interaction handles remain owned by the same runtime; an explicit
session switch or exit cancels them.

## Trigger and retention

`/compact [focus]` requests a main handoff. Before each main or child request, TTC
estimates instructions, tools, attachments, history and output allowance against
the selected model's budget. It compacts before admitting an oversized request.
The adapter estimates native replay occupancy from model-visible content, rather
than treating encrypted transport JSON as literal input tokens. Estimates remain
approximate; endpoint usage counters are kept separately.
A provider context-length rejection is reported directly; TTC does not compact
and retry rejected requests.

Keep complete recent user turns up to `recent_tokens_target`. If the active turn
alone is larger, keep its initiating user/task message and the last two assistant
messages with their complete tool results. Cut only at a completed tool cycle;
never split call/result pairs or discard unresolved calls. The mandatory suffix
may exceed the retention target, but the assembled next request must fit capacity
and next-turn headroom. No safe prefix produces an explicit failure.

## Summary and archives

The shared main/child algorithm freezes a cut, archives it, selects a prefix and
summarizes that prefix in one request without tools. It preserves goals,
constraints, decisions, results, open work and exact details to look up. The
summary request uses the actor's selected model and summary output allowance.
Its input contains only that actor's canonical model-message prefix, with raw
tool arguments/results. UI-only activity and expanded tool views stay in archives.
There is no chunking, summary regeneration or required heading schema. Empty
summaries, invalid tool calls and over-budget input/output are rejected.

Archives are immutable private files under the main lineage:

```text
<data-root>/lineages/<lineage-id>/compactions/<content-hash>.md
<data-root>/lineages/<lineage-id>/compactions/<content-hash>.md.jsonl
```

Main Markdown uses the same dense presentation as `/export`, omitting system
instruction bodies and duplicated request payloads. Its JSONL sidecar preserves
original entry envelopes, exact tool records and instruction snapshots. Child
archives contain only the child's selected messages; `/btw` includes its frozen
main prefix. JSONL envelopes retain actor identity, replay state and attachments.
Source shared history remains independently inspectable. The frozen archive
includes the retained suffix; later arrivals belong to a later archive.

The runtime appends verified archive paths to the summary:

```text
Earlier history: /absolute/path/compactions/<hash>.md
Exact records: /absolute/path/compactions/<hash>.md.jsonl
Search with grep, then read matching lines.
```

Drop opaque provider replay state from the returned context. Resupply full project
instructions and current runtime context at the next request. Summaries never
establish whether old jobs still run; current runtime context supplies that state.

## Handoff

During summarization, other actors may finish tools and append committed history.
Only the compacting actor pauses requests. Main handoff takes the routing gate,
waits for file mutations to settle, and checks the summary, retained messages,
concurrent committed tail, pending notifications and fresh runtime context together.
It aborts if the assembled input cannot fit.

One SQLite transaction creates `{lineage-name}-cont-0`, then `-cont-1`, and so on;
inserts the summary, retained suffix and concurrent tail; and freezes the
predecessor. Copies preserve original source/event identity. The runtime changes
its current session pointer before releasing the gate. Summary-first model input
is a projection, distinct from the shared timeline's commit order. Retained file
checkpoints remain undoable; pre-cut changes form the continuation's undo floor.

Child handoff changes neither the main session nor file tips or undo checkpoints.
It preserves the child ID and read-only policy. Success publishes one committed
`child_compacted` notification containing child/turn IDs, archive references and
estimated context usage, without the summary body. Failed handoffs publish none
and terminate the child turn. `/btw` uses the same algorithm; its events and
answer remain outside main model context.

Successful main handoff refreshes estimated context occupancy immediately. The
previous response's reported usage remains separately labeled; summary inference
contributes once to cumulative run totals, including when its handoff later fails.

## Failure policy

Cancellation, transport interruptions, timeouts, rate limits, temporary server
failures and exhausted disk space leave the previous context usable. A later
request may try again after conditions change. These are not automatic compaction
recovery attempts.

Other summary, budget, invariant or persistence failures invalidate the affected
context. A main failure is persisted and also blocks inference in the running
process if that write fails. TTC cancels its live jobs, timers and pending
interactions. History remains inspectable/exportable; start a new session or
load a different usable session. Reloading the failed context never clears its
failure. A failed child is closed without invalidating the main context.

A failed handoff never switches the main session or freezes its predecessor.
Unused archives are disposable with their lineage. After a process restart,
unfinished work is interrupted; historical jobs, timers and image-click requests
are never revived.
