# OpenAI subscription models

## Catalog and startup

- OpenAI is the sole production module and the default `--provider openai`.
  `internal/providers/openai/module.go` wires its transport, credential owner and scoped
  catalog binding, and registers `--openai-base-url` and `--import-codex-auth`.
  The CLI selects the configured factory and owns `catalog.Manager` policy,
  deadlines and lifecycle. Module configuration uses flags, not a provider JSON
  file; [composition](design.md#adding-a-provider) defines extension points.
- Fetch authenticated `/models` with `client_version=0.159.0`, independent of
  TTC/installed Codex versions. Discovery/selection make no inference requests.
- Deadline: 30 seconds or the caller's shorter limit. Retry transport failures,
  429 and 5xx up to three total attempts with exponential jitter; Retry-After
  overrides delay, capped at 30 seconds. Close bodies before retry. Credential,
  certificate, other status and invalid-catalog errors fail immediately.
- Show only `visibility="list"`, ordered by provider priority; each exact model
  ID is a family. Catalog metadata defines reasoning efforts/descriptions/defaults,
  not name heuristics. Reject unsupported choices or no selectable models. Explicit
  `none` sends reasoning metadata; models without controls show synthetic `none`
  and omit reasoning.
- No startup model flag is required. Private `model-choices.json` remembers
  explicit provider/model/variant choices, including Fast. Save before queuing;
  save failure rejects the choice. Activity/compaction/read-only inspection do
  not change it. Switch-record failure keeps the active model but leaves the
  accepted startup preference.
  Cap the file at 64 KiB, reject invalid input and serialize read-modify-write
  saves across instances with a short file lock.
- The transport implements `catalog.Source`, returning remote `ModelInfo` without
  TTC reserves. Provider-neutral `catalog.Manager` validates it and applies
  application `Policy` to produce independently owned `ModelSpec` snapshots.
- The manager caches raw `ModelInfo`, not resolved budgets, in private `0600`
  `openai-models.json` beside TTC's credentials (4 MiB maximum). Schema v2 is
  scoped to provider, account, endpoint and catalog/client version; account and
  endpoint are stored only as SHA-256 hashes. Other schemas are cache misses,
  not migration inputs. Empty cache paths disable disk caching.
- `Open` validates identity locally, then uses a valid cache without synchronous
  catalog HTTP and starts one bounded refresh. The identity guard can wait for
  another process's credential writer/token refresh until the startup deadline;
  it is not bypassed for latency. Missing,
  mismatched or corrupt caches require synchronous discovery and persistence.
  Corruption warns after replacement. Unsafe/unreadable files and cold persistence
  failures are errors; warm refresh failures warn and retain cached choices.
  `Initial` remains the startup snapshot; `Updates` delivers the refresh outcome
  and stays open until `Close`. `Close` cancels/joins work and closes the channel.
  `--offline-script` bypasses provider construction, catalog discovery and caching.
  This deterministic test mode is not a registered production provider and rejects
  `--login`, explicit `--provider` and all provider-specific flags.
- Background refresh updates picker choices and the disk cache, not the active
  selection or existing requests. An open picker retains its snapshot; submission
  resolves against the latest catalog and fails for removed choices. Explicit
  reselection adopts refreshed metadata even with the same ID and variant.
  `Refresh` cancels/joins the prior worker, discards buffered delivery, rebinds
  current identity and synchronously returns fresh choices, without publishing
  through `Updates`. Account guards cover preparation, cache replacement and
  publication. Plain-mode `/login` clears picker choices and calls `Invalidate`
  before authorization, canceling/joining discovery and discarding old delivery.
  Success then calls `Refresh`; authorization or discovery failure/cancellation
  leaves the picker unavailable. The active selection remains frozen.
- Resolve the startup choice against available metadata. With no saved choice or an
  unavailable model, use the first selectable model/default variant; unavailable
  variants use that model's default. Notify on either unavailable selection.
  `--model ID` uses its default unless `--variant VARIANT` is supplied; `--variant`
  alone overrides the remembered model. Invalid explicit overrides fail.

## Switching and replay

- `/model` or Ctrl+X M selects family then variant; plain mode uses
  `/model MODEL_ID [VARIANT]`. Switch after the current tool batch, before the next
  request, or immediately while idle (including after final response/interruption).
  Latest choice wins; identical full selections add no record.
- Atomically apply selection and inspectable `model_switch` metadata; failure
  keeps the old active model. Save turn-start and per-request selections, linking
  each request to its assistant message. Main switches do not affect children;
  idle follow-ups may explicitly select another supported reasoning variant of
  the child's frozen model.
- Native replay provenance includes provider/base model/codec version. Standard
  and Fast of one base are compatible; other bases strip foreign native items
  from the request copy, preserving stored originals.
- Writable loads keep runtime/CLI choice and record a target-session switch if
  needed. Read-only loads display stored selection without enabling requests.

## Standard and Fast

- Advertised Fast `service_tiers` create `MODEL_ID/fast`; ordinary IDs are Standard.
  Both share reasoning/capabilities. Send the original slug and advertised
  `priority`/`fast` tier; Standard explicitly sends `default`. Subscription routing
  headers match model/tier; record actual tier when reported.
- No advertisement means no Fast choice. Invalid/duplicate choice metadata fails;
  no hardcoded catalog or deprecated speed-tier fallback. See
  [speed availability/usage](https://learn.chatgpt.com/docs/agent-configuration/speed)
  and [requested/actual tiers](https://developers.openai.com/api/docs/guides/fast-mode).

## Context and metering

- Budget from `context_window`, using `max_context_window` only if absent; apply
  `effective_context_window_percent`, not the larger maximum automatically. The
  sidebar shows the latest parent request's resolved budget.
- Estimate each document as `max(4096, original_bytes)` tokens and each image as
  4096 tokens, not base64 transport size. These are heuristics, not expansion
  bounds: compressed text, PDF page images and provider augmentation can cost more.

### Budget metadata

`ModelInfo.Limits` carries endpoint capacities; `catalog.Policy` supplies TTC
reserves when preparing `ModelSpec.Budget`. Never invent remote model limits.

| Token field                | Meaning                                       |
| -------------------------- | --------------------------------------------- |
| `context_limit`            | Effective input-plus-output capacity          |
| `max_output_tokens`        | Endpoint ceiling, or zero when unpublished    |
| `output_allowance`         | Coding output reserve; cap where supported    |
| `estimation_margin`        | Estimation/wire-overhead reserve              |
| `recent_tokens_min`        | Soft minimum for recent model/tool cycles     |
| `recent_tokens_max`        | Tail target cap; unread binary cycle excepted |
| `next_turn_input_reserve`  | Free capacity for new input after compaction  |
| `summary_output_allowance` | Summary reserve; cap where supported          |

Reject unknown variants/options, invalid capacities and invalid reserve policy.
Preparation clamps output/summary allowances to published ceilings and omits
models unable to fit application reserves; no remaining models is an error.
Request admission checks fixed instructions plus required summary/output/headroom.
Subscription transport publishes neither an output
ceiling nor an output-cap request field, so allowances reserve context rather
than cap generation. [Compaction](compaction.md#trigger-and-retention) owns
admission and handoff inequalities, retention defaults and failure behavior.

### Usage accounting

- Preserve reported zero versus unavailable and separate cache writes. Show
  latest successful parent input/cache and cumulative output/reasoning. Run totals
  count each finished parent/child/aside/naming/compaction response once since process
  start or `/new`, including later cache reads; other session changes/compaction
  retain them in memory. Ordinary input = input − reads − writes; output includes reasoning.
  Cache reads/writes are disjoint input subsets; reasoning is an output subset.
  Uncached input subtracts reads only. Show missing usage as coverage, not estimated
  zero; an optional total is unavailable if any reported response omitted it.
- Copy counters under the runtime mutex. Match parent reported input/cache and
  frozen model to the producing request ID. Numerical/percentage occupancy includes
  reserves, with estimates labeled separately from endpoint usage. Compaction
  refreshes occupancy immediately while prior reported usage remains visible.
  Explicit session changes other than `/new` clear parent context usage, not
  all-agent run totals (including `/clear`). Never rebuild those totals from history.
- Cost needs each producing model/tier's rates: ordinary input × input rate +
  reads × cached-input rate + writes × cache-write rate + output × output rate.
  Mixed-model totals determine neither cost nor a subscription bill. See
  [prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching).
- Send stable `prompt_cache_key` and matching `session-id`: main uses its local
  session ID through turns/tools/reloads, continuations get new IDs, children keep
  actor IDs through parent compaction. Naming/compaction suffix the source session
  ID with purpose. Identities expose no paths/credentials; OpenAI rejects empty,
  whitespace/control/non-ASCII IDs before auth/network.
- `store:false` disables server response storage, not caching. Replay locally,
  without `previous_response_id`; stable prefixes/affinity cannot guarantee hits.
  Codex 0.159.2 shares storage, affinity and
  `usage.input_tokens_details.cached_tokens` decoding:
  [requests](https://github.com/openai/codex/blob/rust-v0.159.2/codex-rs/core/src/client.rs),
  [headers](https://github.com/openai/codex/blob/rust-v0.159.2/codex-rs/codex-api/src/requests/headers.rs),
  [usage](https://github.com/openai/codex/blob/rust-v0.159.2/codex-rs/codex-api/src/sse/responses.rs).

## Design references

- TTC follows Codex's subscription catalog/exact efforts/errors, with explicit
  compiled module composition rather than a runtime provider registry:
  [endpoint](https://github.com/openai/codex/blob/2cc65cdd4c7c167f0c7252fcb5165d649c16aca0/codex-rs/codex-api/src/endpoint/models.rs),
  [manager](https://github.com/openai/codex/blob/2cc65cdd4c7c167f0c7252fcb5165d649c16aca0/codex-rs/models-manager/src/lib.rs).
- OpenCode uses a shared catalog/config, OAuth filtering and name heuristics:
  [subscription plugin](https://github.com/anomalyco/opencode/blob/2fa3363c924c5c3e367b84a87ae478296a0ed59b/packages/opencode/src/plugin/openai/codex.ts),
  [variants](https://github.com/anomalyco/opencode/blob/2fa3363c924c5c3e367b84a87ae478296a0ed59b/packages/opencode/src/provider/transform.ts).
- pi's legacy Codex provider uses generated metadata/effort mappings, separately
  from ChatGPT OAuth: [catalog](https://github.com/earendil-works/pi/blob/1b347794e2a630e4359f2584f4eea388145d0ddf/packages/ai/scripts/generate-models.ts),
  [transport](https://github.com/earendil-works/pi/blob/1b347794e2a630e4359f2584f4eea388145d0ddf/packages/ai/src/api/openai-codex-responses.ts).
