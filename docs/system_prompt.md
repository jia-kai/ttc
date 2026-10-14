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
