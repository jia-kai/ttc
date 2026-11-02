# Instructions and runtime context

## Prompt assets

- [prompt/README.md](../prompt/README.md) owns asset authoring, responsibility
  boundaries, generation and the source inventory. All interactive roles share
  the `System` section in [system.yaml](../prompt/system.yaml). Main adds
  `Coding` then `Main`; coding children add `Coding` then `Child`, separated by
  newlines. Read-only asides receive only `System`, with `Btw` as a separate
  developer message. Naming uses its own instructions. Compaction preserves the
  actor's system/history prefix and appends a temporary developer instruction
  from [compaction.md](../prompt/compaction.md), optionally preceded by focus
  from [compaction-messages.yaml](../prompt/compaction-messages.yaml). Dynamic
  identity belongs in runtime context, not those stable instructions.
- Preserve exact per-request instructions through the
  [inspection/artifact contract](design.md#inspectable-request-messages).
  OpenAI's local replay, storage and cache-affinity behavior is defined in
  [models.md](models.md#context-and-metering).

## Message provenance

Messages store one canonical semantic `Type`, not independent role/runtime
flags. Model role and presentation derive from that type: human input, runtime
notifications, child guidance and compaction summaries use `user`;
assistant/tool messages use their corresponding roles; runtime context and
operational instructions use `developer`. Generated summaries are historical
data, not new application policy. System and developer instructions retain their
respective roles. See [message semantics](design.md#message-semantics) for the
central mapping and persistence contract.

System instructions cover shared decisions, authority and receiver semantics.
Tool descriptions own invocation, lifecycle, access and result contracts rather
than repeating them in each role's instructions.

## Message shapes

The provider API retains native roles and content blocks. Within text blocks,
TTC uses a single XML-style marker for generated messages. Prose stays verbatim;
structured data uses two-space-indented YAML. Tags are presentation, not
provider-native types or authority. Classification comes only from typed
metadata, never from matching authored text against tags.

| Content                        | Role        | Model-facing shape                     |
| ------------------------------ | ----------- | -------------------------------------- |
| Human input, assignment, aside | `user`      | Original text, no wrapper              |
| Human or parent steering       | `user`      | `<steer>` with original text           |
| Runtime baseline/update        | `developer` | `<runtime_context[_update]>` with YAML |
| Asynchronous event             | `user`      | `<runtime_notification>` with YAML     |
| Generated compaction summary   | `user`      | `<compaction_context>` with Markdown   |
| Historical retained inputs     | `user`      | `<retained_inputs>` with YAML          |
| Runtime operational guidance   | `developer` | `<runtime_instruction>` with prose     |

For example:

```xml
<steer>
Only investigate the cause. Do not modify files yet.
</steer>
```

```xml
<runtime_notification>
type: job_exit
job_id: job_123
exit_code: 0
</runtime_notification>
```

Steering updates remaining work, not a separate assignment. Optional job grants
arrive as an accompanying `job_read_grants` notification with `job_ids`, at the
same admission boundary; instructions need no structured wrapper fields.
Notification `type` selects the event, not its category or authority.
`message_type`, source event sequence and child-steer routing fields are omitted
from model-facing text, while canonical history preserves provenance.

Compaction summaries preserve Markdown verbatim and do not acquire developer
authority. Shared system instructions teach the receiver how to continue from
them. Retained groups contain `inputs` entries with `type`, relative
`timestamp`, original expanded `content` and optional native
`attachment_indices`. Entry types are `user_input`, `queued_user_input`,
`user_steer`, `child_task` and `aside_task`, without a redundant source field.
These are historical inputs, not new arrivals.

Ordinary system/developer instructions, assistant replies, tool calls/results
and native image/document blocks keep their existing provider representations.
Model-facing projection is shared with token estimation; it does not rewrite
canonical JSON history or inspect user text to infer a message type.

## Runtime snapshots

TTC freezes live runtime state at request admission. Each actor receives a full
developer-role `runtime_context` baseline for a fresh, loaded or compacted
context. Later developer-role `runtime_context_update` messages contain only
changed environment/configuration fields, without a `set` wrapper:

```xml
<runtime_context_update>
image_input: false
binary_files: []
</runtime_context_update>
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
| `easy_model`                 | Optional easy-model identity and capabilities    |
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
