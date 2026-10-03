# OpenAI subscription models

## Catalog and startup

- Fetch authenticated `/models` with `client_version=0.159.0`; this negotiates
  the catalog protocol independently of TTC or installed Codex versions.
- Discovery has a 30-second deadline, shortened by the caller. Retry transport
  failures, 429 and 5xx for at most three total attempts with exponential jitter;
  Retry-After overrides delay, capped at 30 seconds. Close bodies before retry.
  Credential, certificate, other status and invalid-catalog errors fail immediately.
- Only `visibility="list"` entries appear. Provider priority orders families;
  each exact model ID is a family. Reasoning efforts/descriptions/defaults come
  from the catalog, not name heuristics. Reject unsupported selections or catalogs
  without visible models. Explicit `none` sends reasoning metadata; models without
  reasoning controls show synthetic `none` and send no reasoning field.
- Startup needs no model flag. Private `model-choices.json` remembers explicitly
  selected provider/model/variant, including Fast. Save before queuing application;
  save failure rejects the choice. Activity/compaction/read-only inspection do not
  change it. A later switch-record failure preserves the active model while the
  accepted startup preference remains.
- Refresh capabilities/budgets/variants on startup. Without a saved choice, use
  the first selectable model/default variant. Unavailable saved models use that
  catalog default; unavailable variants use the chosen model's default. Show a
  notice for either. `--model ID` uses its default unless `--variant VARIANT` is
  provided; `--variant` alone overrides the remembered model. Invalid explicit
  overrides fail. Discovery/selection makes no inference requests.

## Switching and replay

- `/model` or Ctrl+X M opens family then variant selection; plain mode accepts
  `/model MODEL_ID [VARIANT]`. During work, switch after the current tool batch,
  before the next request. Idle changes apply immediately, including pending
  choices after a final response/interruption. Latest choice wins; no-op choices
  add no record.
- Apply selection and inspectable `model_switch` metadata atomically; failure
  preserves the old active model. Record each request's selection and link its
  assistant message. Main switches leave child selections unchanged; an explicit
  idle follow-up can choose another supported reasoning variant of its frozen model.
- Replay provenance includes provider, underlying model ID and codec version.
  Standard/Fast of the same base model are compatible. Another base model strips
  foreign native items from the request copy, preserving stored originals.
- Writable loads retain the current runtime/CLI choice and record a target-session
  switch if needed. Read-only loads show stored selection and remain inspection-only.

## Standard and Fast

- Advertised Fast `service_tiers` create `MODEL_ID/fast` choices; ordinary IDs are
  Standard. Both share reasoning variants/capabilities. Requests send the original
  slug and advertised `priority`/`fast` tier; Standard explicitly sends `default`.
  Subscription routing headers match model/tier; store actual tier when reported.
- Missing advertisement means no Fast choice. Invalid/duplicate metadata fails;
  there is no hardcoded catalog or deprecated speed-tier fallback. See
  [speed availability/usage](https://learn.chatgpt.com/docs/agent-configuration/speed)
  and the [requested/actual tier contract](https://developers.openai.com/api/docs/guides/fast-mode).

## Context and metering

- Budget from normal `context_window`; use `max_context_window` only if absent.
  Apply advertised `effective_context_window_percent`; do not automatically choose
  the larger maximum. The sidebar shows the latest parent request's resolved budget.
- Endpoint usage distinguishes zero from unavailable. Preserve cache writes
  separately. Run totals add every finished parent/child/aside/naming/compaction
  response once since process start or `/new`, including cache reads on later
  requests. Session changes and compaction retain these in-memory totals.
  Ordinary input is input minus reads/writes; reasoning is already included in
  output. Missing usage appears as coverage, not estimated zero.
- Cost requires each producing model/tier's rates: ordinary input × input rate +
  reads × cached-input rate + writes × cache-write rate + output × output rate.
  Mixed-model token totals alone determine neither cost nor a subscription bill.
  See [prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching).
- Send stable `prompt_cache_key` and matching `session-id`. Main coding uses the
  local session ID through turns/tool boundaries/reloads; continuations get a new
  ID. Children use their actor ID through parent compaction. Naming/compaction
  append their purpose to the source session ID. Providers see an opaque identity;
  OpenAI rejects empty, whitespace/control/non-ASCII IDs before auth/network.
  IDs expose no workspace paths or credentials.
- `store:false` disables server response storage, independently of caching. TTC
  replays locally without `previous_response_id`. Stable prefixes/affinity help
  routing but cannot guarantee hits. Codex 0.159.2 uses the same storage setting,
  affinity header and `usage.input_tokens_details.cached_tokens` decoder:
  [request builder](https://github.com/openai/codex/blob/rust-v0.159.2/codex-rs/core/src/client.rs),
  [headers](https://github.com/openai/codex/blob/rust-v0.159.2/codex-rs/codex-api/src/requests/headers.rs),
  [usage decoder](https://github.com/openai/codex/blob/rust-v0.159.2/codex-rs/codex-api/src/sse/responses.rs).

## Design references

TTC follows Codex's subscription catalog, exact efforts and invalid-choice errors.
OpenCode uses a shared catalog/config plus OAuth filtering and name heuristics;
pi's legacy Codex provider uses generated metadata/effort mappings, separate from
its ChatGPT OAuth route. TTC needs no shared provider registry.

- Codex: [model endpoint](https://github.com/openai/codex/blob/2cc65cdd4c7c167f0c7252fcb5165d649c16aca0/codex-rs/codex-api/src/endpoint/models.rs),
  [manager](https://github.com/openai/codex/blob/2cc65cdd4c7c167f0c7252fcb5165d649c16aca0/codex-rs/models-manager/src/lib.rs).
- OpenCode: [subscription plugin](https://github.com/anomalyco/opencode/blob/2fa3363c924c5c3e367b84a87ae478296a0ed59b/packages/opencode/src/plugin/openai/codex.ts),
  [variants](https://github.com/anomalyco/opencode/blob/2fa3363c924c5c3e367b84a87ae478296a0ed59b/packages/opencode/src/provider/transform.ts).
- pi: [catalog generation](https://github.com/earendil-works/pi/blob/1b347794e2a630e4359f2584f4eea388145d0ddf/packages/ai/scripts/generate-models.ts),
  [legacy transport](https://github.com/earendil-works/pi/blob/1b347794e2a630e4359f2584f4eea388145d0ddf/packages/ai/src/api/openai-codex-responses.ts).
