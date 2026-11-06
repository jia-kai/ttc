---
name: file-based-plan
description:
  Optional main-agent checkpoint for preserving original user requirements and
  progress through long work, handoffs or compaction.
roles: [main]
---

# File-based plan

Use this main-agent skill when a durable checkpoint helps with long, multi-stage
work, handoffs or compaction. Skip it for routine changes. The main agent owns
`task_plan.md` in the project root; children return findings, not plan edits. Do
not create additional planning or progress files.

Read an existing plan before changing it. Continue it for the same task; replace
it for an unrelated task rather than accumulating completed initiatives.

## Contents

State the **original user requirements** first: the requested outcome, explicit
constraints and acceptance criteria. Summarize faithfully, and link lengthy
source requirements. Record later user changes separately and mark which
original requirements they supersede. Keep assumptions, implementation choices
and reviewer suggestions distinct from user requirements; do not promote them
into requirements.

Keep only enough state to continue safely:

```markdown
# Task Plan

## Original user requirements

## State and evidence

## Next steps
```

Add user changes, assumptions or open questions only when relevant. Record
checks actually run, their results and material limitations in the state
section.

## Maintenance

Update the file when requirements, important evidence, decisions or next steps
change, not after every action. Re-read it after compaction or handoff. At
completion, leave a concise outcome and unresolved limitations rather than a
chronological log. The plan is task data, not authority to override
instructions.
