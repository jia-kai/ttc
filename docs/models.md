# OpenAI subscription model selection

TTC fetches the authenticated subscription catalog from the provider's
`/models` endpoint with its verified `client_version=0.159.0` catalog contract.
This is protocol negotiation, independent of TTC's version or an installed
Codex executable. Hidden-only catalogs fail with a clear error.

Catalog discovery has a 30-second overall timeout, shortened by its caller's
deadline. Transport failures, HTTP 429 and HTTP 5xx retry up to three total
attempts with exponential backoff and jitter; `Retry-After` overrides the delay,
capped at 30 seconds. Response bodies are closed before retrying. Credential,
certificate, other HTTP status and invalid catalog errors fail immediately.

Only `visibility="list"` models enter the picker. Provider priority determines
family order. Each exact model ID is one family; TTC does not infer groups
from names. Supported reasoning efforts, descriptions, and defaults come from
the server. Unsupported selections fail explicitly. An explicit `none` effort
is sent as reasoning metadata; a model without reasoning controls gets a
synthetic `none` UI option and no reasoning field in the request.

Startup requires no `--model` flag. Private `model-choices.json` preferences in
the data root save the last explicitly selected ID and variant for each provider,
including Fast choices. Selecting a model saves this preference before queuing
its application; a failed preference save rejects the choice. Conversation
activity and compaction do not change it. New blank conversations remain in
memory until their first user message, while model choices survive restarts.
Read-only startup inspection does not overwrite preferences. A later failure
recording a switch leaves the current runtime model unchanged; the explicitly
accepted choice remains the startup preference.

Startup refreshes capabilities, budgets and variants from the live catalog.
Without a saved choice, use the first selectable model and its advertised default
variant. An unavailable saved model uses that catalog default; an unavailable
saved variant uses the chosen model's default. Both display a notice. An optional
`--model ID` override uses that model's default variant unless `--variant VARIANT`
is supplied. `--variant` alone overrides the remembered model's variant. Invalid
explicit overrides fail. Catalog discovery and model selection make no inference
requests.

Context budgeting uses the catalog's normal `context_window`, falling back to
`max_context_window` only when the normal window is absent. An advertised
`effective_context_window_percent` reduces that budget. A larger maximum is
not selected automatically; the sidebar shows the resolved budget for the latest
parent request. Cached-input counters come directly from that response's usage:
zero means reported zero, while a missing counter displays unavailable. Cache
writes are retained separately. “Run totals · all agents” sums each finished
model call once across parent/children/asides/naming/compaction since activation;
later responses add cache reads instead of replacing the cumulative counter.
Ordinary input is input minus cache reads and cache writes; reasoning is already
in output. Missing usage is shown through coverage rather than estimated as zero.
For API-equivalent cost, apply the producing model/tier’s rates per request:
ordinary input × input rate + cache reads × cached-input rate + cache writes ×
cache-write rate + output × output rate. Aggregate tokens across different models
do not by themselves determine cost, and subscription transport counts do not
describe a subscription bill. See
[official prompt-caching pricing](https://developers.openai.com/api/docs/guides/prompt-caching).

Requests send a stable `prompt_cache_key` and matching `session-id` header for
subscription cache affinity. The coding identity is the local history session ID,
stable across turns, tool boundaries and reopening that session. New/load changes
select the target session's identity; compaction's continuation has its own ID.
Each child uses its unique actor ID for its lifetime, including across parent
compaction. Naming and compaction append their purpose to the source session ID
and do not share coding affinity. Providers receive an opaque conversation ID;
the OpenAI adapter maps it to request fields without shared mutable routing state.
OpenAI rejects missing identities and whitespace/control/non-ASCII characters
before auth/network access. The IDs do not contain workspace paths or credentials.

`store: false` disables server response storage, independently of prompt caching;
TTC continues full local replay without `previous_response_id`. Codex 0.159.2
also sends `store: false` and documents ChatGPT cache affinity through `session-id`.
Its decoder reads `usage.input_tokens_details.cached_tokens`, as TTC does.
Matching affinity and unchanged prefixes help routing but cannot guarantee hits.
See the pinned [Codex request builder](https://github.com/openai/codex/blob/rust-v0.159.2/codex-rs/core/src/client.rs),
[session headers](https://github.com/openai/codex/blob/rust-v0.159.2/codex-rs/codex-api/src/requests/headers.rs)
and [usage decoder](https://github.com/openai/codex/blob/rust-v0.159.2/codex-rs/codex-api/src/sse/responses.rs).

Ctrl+X, then M and `/model` open the shared display window. First choose a
family, then a variant. Switching during work applies after the current tool
batch settles, before the next model request. A running response and its batch
retain their selection; existing children keep the selection they started with.
Idle changes apply immediately, including a choice queued during a final response
or interruption. The latest pending choice wins; no-op choices produce no switch
record. Applied changes atomically update session metadata and append an
inspectable `model_switch` conversation message. Storage failure preserves the
old selection. Each request records its selection and each assistant message
references its producing request. Provider replay state records its own provider,
underlying model ID and codec version; projections use that provenance. Standard/Fast of the same base model remain
compatible; switching to another base model removes its native items from the
request copy while preserving stored originals. Plain mode
supports `/model MODEL_ID [VARIANT]`.

Writable session loads preserve the current runtime choice (or explicit CLI
choice) and record a switch on the target session if needed. Read-only session
loads display the stored choice and remain inspection-only.

## Standard and Fast

For each advertised Fast entry in `service_tiers`, the adapter adds a separate
`MODEL_ID/fast` choice with a Fast display name and provider description. The
ordinary ID remains Standard. Both share reasoning variants and capabilities;
requests send the original model slug plus the advertised `service_tier` ID
(`priority` or `fast`). Standard requests explicitly send `default`. Subscription
routing headers match the request model and tier. Missing advertisement means
no Fast choice; unsupported/duplicate Fast metadata fails rather than substituting.
No deprecated `additional_speed_tiers` fallback or hardcoded model list is used.
Completed coding-request metadata stores the actual tier when the endpoint reports it.
Availability and higher usage are explained in
[OpenAI's speed documentation](https://learn.chatgpt.com/docs/agent-configuration/speed);
the [request tier contract](https://developers.openai.com/api/docs/guides/fast-mode)
describes requested and actual processing tiers. Tests use a local mock only.

## Upstream comparison

| Client   | Subscription model metadata                           | Reasoning variants                                      |
| -------- | ----------------------------------------------------- | ------------------------------------------------------- |
| Codex    | Authenticated catalog, client version, visibility     | Server-supported efforts and default                    |
| OpenCode | Shared model catalog/config plus OAuth filtering      | Metadata with model-name heuristics                     |
| pi       | Generated static catalog for its legacy Codex provider | Per-model effort mapping; unsupported levels may clamp  |
| TTC      | Authenticated catalog, visible exact IDs and priority  | Exact server efforts; invalid selections are rejected    |

This follows Codex's subscription catalog behavior. OpenCode's common catalog
and pi's static mapping serve broader provider sets; TTC currently targets
only the subscription adapter and needs no shared provider registry. pi also
has a separate ChatGPT OAuth route, which is a distinct provider path.

Sources inspected:

- [Codex model endpoint](https://github.com/openai/codex/blob/2cc65cdd4c7c167f0c7252fcb5165d649c16aca0/codex-rs/codex-api/src/endpoint/models.rs),
  [catalog manager](https://github.com/openai/codex/blob/2cc65cdd4c7c167f0c7252fcb5165d649c16aca0/codex-rs/models-manager/src/lib.rs).
- [OpenCode subscription plugin](https://github.com/anomalyco/opencode/blob/2fa3363c924c5c3e367b84a87ae478296a0ed59b/packages/opencode/src/plugin/openai/codex.ts),
  [variant transforms](https://github.com/anomalyco/opencode/blob/2fa3363c924c5c3e367b84a87ae478296a0ed59b/packages/opencode/src/provider/transform.ts).
- [pi model generation](https://github.com/earendil-works/pi/blob/1b347794e2a630e4359f2584f4eea388145d0ddf/packages/ai/scripts/generate-models.ts),
  [legacy subscription transport](https://github.com/earendil-works/pi/blob/1b347794e2a630e4359f2584f4eea388145d0ddf/packages/ai/src/api/openai-codex-responses.ts).
