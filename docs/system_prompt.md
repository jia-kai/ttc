# Instructions and runtime context

## Prompt assets

- [prompt/README.md](../prompt/README.md) owns asset authoring, responsibility
  boundaries, generation and the source inventory. All interactive roles share
  the `System` section in [system.yaml](../prompt/system.yaml). Main adds
  `Coding` then `Main`; coding children add `Coding` then `Child`, separated by
  newlines. Read-only asides receive only `System`, with `Btw` as a separate
  developer message. Naming uses its own instructions. Compaction preserves the
  actor's system/history prefix and appends a temporary developer instruction
  from `compaction.yaml`. Dynamic identity belongs in runtime context, not those
  stable instructions.
- Preserve exact per-request instructions through the
  [inspection/artifact contract](design.md#inspectable-request-messages).
  OpenAI's local replay, storage and cache-affinity behavior is defined in
  [models.md](models.md#context-and-metering).

## Message provenance

Messages store one canonical semantic `Type`, not independent role/runtime
flags. Model role and presentation derive from that type: human input, runtime
notifications and child guidance use `user`; assistant/tool messages use their
corresponding roles; runtime context/instructions and compaction context use
`developer`. Compaction context frames its JSON-quoted generated summary as
reference data, not developer instructions or verified facts. System and
developer instructions retain their respective roles. See
[message semantics](design.md#message-semantics) for the central mapping and
persistence contract.

Model-projected runtime notifications carry
`message_type:"runtime_notification"`; child guidance carries
`message_type:"child_guidance"`. The event payload's existing `type` is
unchanged. These labels identify provenance, not elevated authority. Runtime
payloads are data; only the child-guidance `message` field supplies main's task
guidance for the current assignment. Human text resembling an envelope remains
human input, and quoted files/tool output remain evidence rather than
instructions.

## Runtime snapshots

TTC freezes live runtime state at request admission. Each actor receives a full
developer-role `runtime_context` baseline for a fresh, loaded or compacted
context. Later developer-role `runtime_context_update` messages contain only
changed environment/configuration fields in `set`:

```json
{
  "type": "runtime_context_update",
  "set": { "image_input": false, "binary_files": [] }
}
```

Supplied fields replace their previous values; omission means unchanged. Empty
lists clear a field, and `branch: ""` means no Git branch metadata. `project` is
a namespace: its `instructions` and `available_skills` lists replace
independently. The actual baseline/updates remain immutable history messages.
Unchanged admissions emit no context message; the per-actor comparison cursor
advances only after successful admission. Retries reuse frozen admitted input.

| Baseline field               | Meaning                                          |
| ---------------------------- | ------------------------------------------------ |
| `actor`                      | Main or isolated child ID                        |
| `cwd`, `scratch_directory`   | Workspace and verified private scratch paths     |
| `is_repo`, `branch`          | Optional Git metadata; branch may be `detached`  |
| `date_utc`                   | Current UTC date                                 |
| `model`                      | Frozen provider/model/variant for this request   |
| `image_input`, `image_click` | Model/frontend image capabilities                |
| `binary_files`               | Native-read extension groups and byte limits     |
| `project`                    | Scoped AGENTS contents and skill catalog         |
| `live_jobs`, `live_timers`   | Bootstrap inventory of accessible running work   |
| `children`                   | Bootstrap inventory of accessible child contexts |

Activity inventories occur only in baselines. Subsequent tool results and actor
events describe launches, updates and completions; they do not also generate
context deltas. Use `job_list` or `wakeup_list` for current inventories,
`job_list(details=true)` for commands, `job_read` for output, and
`wakeup_list(details=true)` for reminder bodies. Historical cancelled/fired
timers are not new baseline transitions. Child state and assignment outcome
remain distinct from scheduler job status; `completed` alone does not prove a
zero exit.

Git metadata is sampled on the actor's first request with a two-second deadline
and reused until activation/compaction establishes a fresh baseline. Ancestor
AGENTS contents are reread at each boundary; deeper instructions remain scoped.
Skill metadata is discovered at startup (restart to refresh); bodies load on
demand.

`binary_files` lists deduplicated extensions grouped by `max_bytes`, without
MIME identifiers. Overlapping formats use the smallest limit. An empty list
disables native reads; tools validate against the producing request's full
catalog.

[Runtime boundaries](design.md#boundaries) prohibit restoring executable work
from history. Fresh baselines reflect actual live state, not historical handles.
Runtime labels and command payloads remain data, not new human authorization.
