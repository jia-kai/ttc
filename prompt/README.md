# Embedded LLM text

All TTC-authored text sent to models belongs here: instructions, tool descriptions,
validation/recovery guidance, runtime warnings, fallback notices and message
framing. Go owns behavior and supplies dynamic values, not the authored prose.
UI-only wording, protocol/schema keys, status values, external diagnostic payloads
and user/project content are not prompt assets. TTC-authored diagnostic captions
and validation text are assets, even when wrapped by another error. Synthetic test
conversations stay in fixtures.

## Sources

- `system.md`: stable coding instructions shared by main and children.
- `child.md`: child-only suffix; do not duplicate main instructions.
- `btw.md`: read-only aside instructions.
- `recovery.md`: continuation warning after a partial coding failure.
- `retained-input.md`: provenance guidance for retained human input.
- `compaction.yaml`: summary instructions, input with two `%s` slots
  (focus/transcript), and links with three (summary/Markdown/JSONL archives).
- `naming.yaml`: title instructions and output-token/deadline limits.
- `tools.yaml`: descriptions keyed by tool name, with reusable notes.
- `runtime.yaml`: failed-child caller guidance and fallback messages.
- `*-messages.yaml`: tool/session guidance, diagnostic prose, and shared
  attachment/archive/presentation framing. Each key is an exported Go constant
  name; values are nonempty UTF-8 strings.
  Preserve printf slots and whitespace; static constants let `go vet` check callers.
- `skills/*/SKILL.md`: bundled skill instructions, embedded by `skills/embed.go`.
  External project/user skills retain their own source files and precedence.

## Build and validation

Edit assets, then run `make prompts`. `make build`, `make test` and `make check`
generate git-ignored `internal/prompts/assets_generated.go` before compiling.
The executable needs no prompt files or YAML parser at runtime.

Generation validates text, asset names, duplicate/reserved symbols, YAML fields,
tool coverage and naming limits. Markdown bytes are preserved and generated Go
source is deterministic and written atomically. Bundled skills use Go embedding.
Regression checks reject prose literals at tool-error sinks; audits must also
trace indirect model-input paths when adding runtime features.

Keep system instructions focused on decisions and tool descriptions focused on
parameter semantics, defaults and results. Recoverable errors should explain how
to adjust the call rather than expanding descriptions into lists of edge cases.
