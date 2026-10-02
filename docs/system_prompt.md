# Instructions and runtime context

## Prompt assets

- [prompt/](../prompt/README.md) is the source for embedded LLM instructions,
  tool descriptions, reusable tool notes and naming configuration.
- [system.md](../prompt/system.md) supplies stable coding instructions. Children
  append [child.md](../prompt/child.md); identity belongs in runtime context.
- System instructions guide model decisions: project scope, verification,
  dependency ordering, delegation and output. Scheduling, message delivery and
  UI mechanics belong to the harness. Important parameter semantics and results
  belong to tool descriptions; schemas carry types and required fields. System
  guidance keeps parameters only for important choices such as child persistence.
- Naming, compaction and `/btw` use separate assets in the same directory.
- `make build`, `make test` and `make check` regenerate git-ignored Go assets;
  prompt files are unnecessary at runtime.
- OpenAI requests send `store: false` and complete local context, without
  `previous_response_id`. Cache hits come from endpoint-reported usage.
- The TUI shows one coding-prompt placeholder per actor and distinct internal
  prompts. Exact instruction snapshots remain inspectable for each request.

## Runtime snapshots

Append a labeled `developer` message (`type: runtime_context`) at request
boundaries only when state, project instructions or observed transitions change.
Old snapshots stay immutable; the latest snapshot owns current live state.

| Field                            | Meaning                                                       |
| -------------------------------- | ------------------------------------------------------------- |
| `actor`                          | Main or isolated child actor ID                               |
| `working_directory`              | Workspace path                                                |
| `scratch_directory`              | Verified private scratch path                                 |
| `date_utc`                       | Current UTC date                                              |
| `model`                          | Frozen provider/model/variant for this request                 |
| `image_input`, `image_click`      | Model and frontend image capabilities                         |
| `live_jobs`                      | Actor-accessible running metadata, without captured output    |
| `live_timers`                    | Scheduled timers, without reminder bodies                     |
| `children`                       | Accessible running or idle child contexts                     |
| `changes_since_previous_request` | Job outcomes and timer transitions/firing counts               |
| `project`                        | Scoped AGENTS contents and skill metadata                     |

- Each actor advances its cursor only after successful admission. Initial,
  reloaded and compacted contexts receive a fresh snapshot.
- An unchanged boundary adds no snapshot or UI row; request recording and
  pending message delivery still proceed.
- Empty live arrays mean no current work. A completed job reports its outcome,
  including failed shell exits; repeating timers report new firing counts.
- Transitions and project updates are compared separately; clearing them does
  not create another snapshot.
- Omitted `project` retains the last value; an explicit empty project clears it.
  Compaction resupplies project context because the initial snapshot may be archived.
- History never restores jobs, timers or pending image interactions. Runtime
  labels and command text are data, without new human authorization.
- Ancestor AGENTS contents are reread at each boundary. Deeper instructions
  remain scoped. Skill metadata is discovered at startup; restart to refresh
  the catalog. Skill bodies load on demand.
- Runtime context is budgeted before requests and continuation creation. Tool
  definitions stay in a stable, structured provider field.
