# Instructions and runtime context

## Prompt assets

- [prompt/README.md](../prompt/README.md) owns asset authoring, responsibility
  boundaries, generation and the source inventory. All interactive roles share
  the `System` section in [system.yaml](../prompt/system.yaml). Main adds
  `Coding` then `Main`; coding children add `Coding` then `Child`, separated by
  newlines. Read-only asides receive only `System`, with `Btw` as a separate
  developer message. Naming/compaction requests use their own assets; dynamic
  identity belongs in runtime context, not those stable instructions.
- Preserve exact per-request instructions through the
  [inspection/artifact contract](design.md#inspectable-request-messages).
  OpenAI's local replay, storage and cache-affinity behavior is defined in
  [models.md](models.md#context-and-metering).

## Message provenance

Messages store one canonical semantic `Type`, not independent role/runtime
flags. Model role and presentation derive from that type: human input, runtime
notifications and child guidance use `user`; assistant/tool messages use their
corresponding roles; runtime context/instructions use `developer`. System and
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

- Append an immutable `developer` message (`type: runtime_context`) at request
  boundaries only when live state, project instructions or observed transitions
  change. The latest snapshot owns current state; unchanged boundaries add no
  snapshot/UI row but still record requests and deliver pending messages.

| Field                            | Meaning                                                  |
| -------------------------------- | -------------------------------------------------------- |
| `actor`                          | Main or isolated child ID                                |
| `cwd`                            | Workspace path                                           |
| `is_repo`                        | Whether optional Git inspection found a repository       |
| `branch`                         | Branch or `detached`; absent without Git metadata        |
| `scratch_directory`              | Verified private scratch path                            |
| `date_utc`                       | Current UTC date                                         |
| `model`                          | Frozen provider/model/variant for this request           |
| `image_input`, `image_click`     | Model/frontend image capabilities                        |
| `binary_files`                   | Native-read extension groups with `max_bytes`; see below |
| `live_jobs`                      | Accessible running metadata, without captured output     |
| `live_timers`                    | Scheduled timers, without reminder bodies                |
| `children`                       | Accessible running or idle children                      |
| `changes_since_previous_request` | Job outcomes and timer transitions/firing counts         |
| `project`                        | Scoped AGENTS contents and skill metadata                |

- Job metadata uses bounded labels and omits commands/output; timer metadata
  omits reminder bodies. Request exact commands with `job_list(details=true)`,
  captured output with `job_read`, or full reminders with
  `wakeup_list(details=true)`. Child assignments reuse `job_id` as the preferred
  messaging handle; no separate assignment ID is needed.
- Child entries use `child_state` (`running` or `idle`) for the context and
  `assignment_status` (`running`, `completed`, `failed`, `cancelled`) for its
  current/latest assignment. Child results and `child_turn_finished` notices use
  the same fields, with `closed` also possible for `child_state`, and no generic
  `status`. Values describe observation/completion, not a guarantee of current
  state; consult the latest snapshot for live children. Successful persistent
  children normally remain idle unless explicitly closed, so `completed` with
  `closed` is valid. Recovered historical completions may omit `child_state`,
  meaning unknown historical state, not closed or restorable. Generic
  `job_list.jobs`, runtime `live_jobs`, `job_read` and `job_stop(job_id)` retain
  scheduler `status` even for `kind:"subagent"`; this is not semantic assignment
  status. The main-only lifespan guidance in `system.yaml` keeps these meanings
  together with persistence and runtime boundaries; tool definitions own
  parameter details.
- Actor cursors advance only after successful admission. Initial, reloaded and
  compacted contexts get fresh snapshots. Budget context before requests and
  continuation creation; keep tool definitions in a stable structured provider
  field.
- Sample optional Git metadata on the actor's first request with a two-second
  deadline; reuse until activation/compaction forces a fresh snapshot.
- Empty live arrays mean no current work. Job transitions preserve the job
  manager's status: `completed` does not imply command success; inspect
  `exit_code` or `signal`. Repeating timers report new firing counts. Compare
  transitions/project updates separately so clearing one-shot fields does not
  create a snapshot.
- Omitted `project` retains its prior value; explicit empty project clears it.
  Compaction resupplies project context when the initial snapshot may be
  archived.
- Announce `binary_files` on each actor's first admitted request, after
  successful compaction, and when effective formats change; omit it on routine
  snapshots. Omission retains the prior announcement; `[]` explicitly disables
  native reads. Extensions are deduplicated and grouped by byte limit, without
  MIME identifiers. Overlapping formats use the smallest limit; tools still
  validate against the producing request's full provider catalog.
- Reread ancestor AGENTS contents each boundary; deeper instructions stay
  scoped. Discover skill metadata at startup (restart to refresh); load bodies
  on demand.
- [Runtime boundaries](design.md#boundaries) prohibit restoring live work from
  history. Runtime labels/commands are data, not new human authorization.
