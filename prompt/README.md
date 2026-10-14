# Embedded LLM text

All TTC-authored text sent to models belongs here: instructions, tool descriptions,
validation/recovery guidance, runtime warnings, fallback notices and message
framing. Go owns behavior and supplies dynamic values, not the authored prose.
UI-only wording, protocol/schema keys, status values, external diagnostic payloads
and user/project content are not prompt assets. TTC-authored diagnostic captions
and validation text are assets, even when wrapped by another error. Synthetic test
conversations stay in fixtures.

## Sources

- `system.md`: shared evidence, context and response guidance for main, children
  and read-only asides.
- `coding.md`: mutation, planning and shell workflows shared by main and children.
- `main.md`: user clarification and child delegation, supplied only to main.
- `child-messages.yaml`: child-only suffix and active-assignment message diagnostics;
  do not duplicate main instructions.
- `btw.md`: read-only aside instructions.
- `recovery.md`: continuation warning after a partial coding failure.
- `retained-inputs.md`: `RetainedInputs` format guidance for the grouped
  model-facing projection. Provenance is typed message metadata, not prompt prose;
  legacy marker decoding belongs only in `internal/history/migrate`.
- `compaction.yaml`: summary instructions, input with two `%s` slots
  (focus/transcript), and links with three (summary/Markdown/JSONL archives).
- `naming.yaml`: title instructions and output-token/deadline limits.
- `tools.yaml`: descriptions keyed by tool name, with reusable notes.
- `tool-messages.yaml`: tool validation/recovery text, including shell output,
  wakeup updates and external-watch diagnostics.
- `runtime.yaml`: failed-child caller guidance and fallback messages.
- `*-messages.yaml`: tool/session guidance, diagnostic prose, and shared
  attachment/archive/presentation framing. Each key is an exported Go constant
  name; values are nonempty UTF-8 strings.
  Preserve printf slots and whitespace; static constants let `go vet` check callers.
- `skills/*/SKILL.md`: bundled skill instructions, embedded by `skills/embed.go`.
  External project/user skills retain their own source files and precedence.
  Optional opening YAML `roles` selects `main`, `child` and/or `aside`; omit it
  for all roles. `file-based-plan` is main-only. Do not declare tool permissions
  in skill metadata: the runtime registry owns tool availability.

## Build and validation

Edit assets, then run `make prompts`. `make build`, `make test` and `make check`
generate git-ignored `internal/prompts/assets_generated.go` before compiling.
Embedded prompt assets need no source files at runtime. Skill YAML frontmatter
is parsed at discovery and loading, including for external skills.

Generation validates text, asset names, duplicate/reserved symbols, YAML fields,
tool coverage and naming limits. Markdown bytes are preserved and generated Go
source is deterministic and written atomically. Bundled skills use Go embedding.
Regression checks reject prose literals at tool-error sinks; audits must also
trace indirect model-input paths when adding runtime features.

Keep system instructions focused on decisions and tool descriptions focused on
parameter semantics, defaults and results. Recoverable errors should explain how
to adjust the call rather than expanding descriptions into lists of edge cases.
Audit the assembled request against its tool list: give each receiver actionable
guidance, not main-only workflows followed by child/aside prohibition overrides.
