# Task Plan

## Goal

Add `make full-test` as the complete automated test entrypoint.

## Assumptions

- Include Go tests/race/vet, PTY integration, rail, Python unit tests, Kitty
  screenshots and math protocol/color-fallback modes; exclude manual demos and benchmarks.
- Optional graphics/math dependencies must be installed separately. The target
  must fail, not silently skip an entire requested suite.
- Preserve existing targets and the user's concise README; add only a command line
  there, with prerequisite details in tests/README.md.

## Risks

- An aggregate must remain sequential under `make -j` and avoid rebuilding the
  executable while another suite is using it.
- Repository ./ttc is a read-only mount; verify execution in a scratch copy,
  without bypassing or changing that mount.

## Implementation Steps

- [x] Add a phony serialized aggregate covering all automated suites/modes.
- [x] Document the command and complete-suite prerequisites concisely.
- [x] Verify command coverage, failure propagation and actual suite execution.
- [x] Obtain independent code/docs/requirements/cleanup review and address findings.

## Validation

- Mocked `make -j8 full-test`: all 26 commands run in order, one build, no
  concurrent commands, and injected Go/Python failures stop the aggregate.
- Actual `make -j8 full-test`: passed every suite in the source-identical scratch
  workspace `/tmp/ttc/1000/full-test-workspace-dE3iRw`, without changing the
  repository executable mount. Complete log: `/tmp/ttc/1000/full-test-final.log`.
- Fixed the existing visual driver's Escape/slash input race by observing
  inspector dismissal before sending the next command. Direct and tmux Kitty
  modes passed after the fix.
- Repeated independent final code/docs/requirements/cleanup review: no findings.
- Dry run, Python syntax, whitespace and local documentation-link checks: passed.

Use `/tmp/ttc/1000/full-test-*` for scratch workspace and logs.

## Notes

- Existing uncommitted rail refactor and user README edits are baseline and must remain intact.
- Do not install system packages or run manual/interactive demos from the target.
