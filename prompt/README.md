# Embedded LLM prompts

Edit assets here, then run `make prompts`. `make build`, `make test`, and
`make check` generate `internal/prompts/assets_generated.go` first. The generated
file is git-ignored; the executable needs no prompt files or YAML parser at runtime.

- `system.md`: stable coding instructions, shared by main and children.
- `child.md`: child-only suffix; never duplicate the main instructions.
- `btw.md`: read-only aside instructions.
- `compaction.yaml`: summary `instructions`, `input` with two `%s` slots
  (focus/transcript), and `links` with three (summary/Markdown/JSONL archives).
- `naming.yaml`: title instructions and output-token/attempt/deadline limits.
- `tools.yaml`: descriptions keyed by exact tool name, with optional reusable notes.

System prompts guide decisions and refer to tools by name. Tool descriptions
explain important parameter semantics, defaults and results; schemas carry types
and required fields. Keep UI and harness mechanics out of model instructions.
Recoverable errors explain how to adjust the call instead of making descriptions
enumerate edge cases.

Generation validates UTF-8, required assets, YAML fields, tool coverage and naming
limits. It preserves Markdown bytes and writes deterministic Go source atomically.

Dynamic values, parameter schemas, result labels and recoverable validation errors
stay with their Go behavior. User/project instructions and loaded skills retain
existing sources; bundled skills are authored in `default-skills/*/SKILL.md`.
Synthetic test conversations belong to their fixtures.
