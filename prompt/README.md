# Embedded LLM text

All TTC-authored text deliberately sent to models belongs here: instructions,
tool descriptions, validation/recovery guidance, runtime warnings, fallback
notices and message framing. Go owns behavior and supplies dynamic values, not
the authored prose. UI-only wording, protocol/schema keys, status values,
external diagnostic payloads and user/project content are not prompt assets.
TTC-authored diagnostic captions and validation text are assets when their
delivery path includes model input, even when wrapped by another error. Inline
UI-only captions, archive formatting, CLI-only diagnostics and server-only
protocol replies in their owning Go code. A model's later reading of an export
does not make its presentation text a prompt asset. Shared captions deliberately
included in child feedback remain assets. Synthetic test conversations stay in
fixtures.

## Sources

- `system.yaml`: role instructions. Main receives `System` + `Coding` + `Main`;
  coding children receive `System` + `Coding` + `Child`, separated by newlines.
  Read-only asides receive `System`, with `Btw` as a separate developer message.
- `child-messages.yaml`: active-assignment message diagnostics.
- `recovery.md`: continuation warning after a partial coding failure.
- `retained-inputs.md`: `RetainedInputs` format guidance for the grouped
  model-facing projection. Provenance is typed message metadata, not prompt
  prose; legacy marker decoding belongs only in `internal/history/migrate`.
- `compaction.yaml`: summary instructions, input with two `%s` slots
  (focus/transcript), and handoff with one (summary plus on-demand history
  export guidance).
- `naming.md`: title instructions. Token/deadline limits belong to session code.
- `tools.yaml`: descriptions keyed by tool name, with reusable notes.
- `tool-messages.yaml`: tool validation/recovery text, including shell output,
  wakeup updates and external-watch diagnostics.
- `runtime.yaml`: failed-child caller guidance and fallback messages.
- `*-messages.yaml`: tool/session guidance, model-facing diagnostics and input
  framing. Each key is an exported Go constant name; values are nonempty UTF-8
  strings. Preserve printf slots and whitespace; static constants let `go vet`
  check callers.
- `skills/*/SKILL.md`: bundled skill instructions, embedded by
  `skills/embed.go`. External project/user skills retain their own source files
  and precedence. Optional opening YAML `roles` selects `main`, `child` and/or
  `aside`; omit it for all roles. `file-based-plan` is main-only. Do not declare
  tool permissions in skill metadata: the runtime registry owns tool
  availability.

## Build and validation

Use folded YAML blocks (`>`) for source-wrapped prose. Use literal blocks (`|`)
when line breaks are part of the template or structured content; compaction
input and handoff templates use `|-` to preserve layout without a trailing
newline. The generator embeds decoded YAML strings without extra normalization.

Edit assets, then run `make prompts`. `make build`, `make test` and `make check`
generate git-ignored `internal/prompts/assets_generated.go` before compiling.
Embedded prompt assets need no source files at runtime. Skill YAML frontmatter
is parsed at discovery and loading, including for external skills.

Generation validates text, asset names, duplicate/reserved symbols, YAML fields,
and tool coverage. Markdown bytes are preserved; generated Go source is
deterministic and written atomically. Bundled skills use Go embedding.
Regression checks reject prose literals at tool-error sinks; audits must also
trace indirect model-input paths when adding runtime features.

Keep system instructions focused on decisions and tool descriptions focused on
parameter semantics, defaults and results. Recoverable errors should explain how
to adjust the call rather than expanding descriptions into lists of edge cases.
Audit the assembled request against its tool list: give each receiver actionable
guidance, not main-only workflows followed by child/aside prohibition overrides.
