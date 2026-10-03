# Task Plan

## Goal

Implement `ttc rail`: one persistent filesystem sandbox and tmux server per
canonical workdir, automatic attach/create, and `--list` without a chooser.

## Decisions

- Linux/Bubblewrap/tmux; shared host networking, private PID/UTS namespaces and
  `{hostname}-ttc` hostname. Payload runs as the invoking user, not host root.
- Global `${XDG_CONFIG_HOME:-~/.config}/ttc/rail.json` and local
  `<workdir>/ttc-rail.json`. Additive allow/deny lists; denies win. Project
  overrides matching ordinary allows. Docker requires global authorization.
- Read-only host system/shell/tmux config; writable workdir and existing TTC
  data/cache exceptions; private home/temp/run. Config files read-only.
- Detach preserves instances; last tmux session exit tears down the sandbox.
  Runtime registry is separate from TTC history. Config edits require restart.
- Paths expand `~`, not shell expressions; relative config paths resolve against
  the config file's parent. Denies mask container destination paths.
- Inside tmux (nonempty `TMUX`), warn and list rather than attach/create;
  explicit `--list` remains quiet.

## Risks

- Host user-namespace policy may prevent running Bubblewrap; never fall back.
- Docker bypasses filesystem isolation; document prominently.
- Symlinked configs and nested mounts must not open read-only/denied paths.
- Concurrent creation, stale sockets, PID reuse, inherited descriptors and
  supervisor shutdown must be handled deliberately.

## Steps

- [x] Diagnose reported startup timeout with the user's shell/tmux configuration.
- [x] Fix the root cause and preserve useful startup diagnostics; add regression.
- [x] Re-run focused checks and obtain independent review of the fix.
- [x] Implement strict config parsing/merge and focused tests.
- [x] Implement mount plan, defaults, deny ordering and focused tests.
- [x] Implement CLI, runtime registry, persistent supervisor and tmux lifecycle.
- [x] Update concise setup/behavior docs and repository guidance.
- [x] Add a bundled `ttc-config` skill for agent-assisted TTC configuration.
- [x] Run unit/race/vet/build and headless tmux/sandbox workflows where supported.
- [x] Independent correctness/documentation/burden/coverage review; fix findings.

## Validation

Passed `make test`, `make check` (race tests and vet), build, the full
`make integration` PTY suite, and `make rail-integration` with real Bubblewrap.
Rail checks include detach/reattach/concurrency/cleanup, terminal resize and
interrupted clients, mount/deny protection, shared networking, offline TTC
startup and tmux client replacement containment. No checks were blocked.
Final logs and PTY artifacts are retained under `/tmp/ttc/1000/rail-final-*`.
Startup fix passed full unit tests, focused race/vet checks, corrected real PTY
integration, and startup/attach/last-session cleanup with the user's actual
Zsh/tmux config. Independent review found no blocking issues. Logs are under
`/tmp/ttc/1000/rail-startup-debug`; the binary was rebuilt.

## Notes

Foreground tmux loads configuration only after a native client connects. The
in-sandbox IPC service now starts independently and owns the foreground tmux
child, avoiding config/client circular startup. Probes never auto-create servers;
startup deadlines report the last probe error. Tests wait for rail readiness
before connecting any host-native fixture clients. No user configs were changed.

The user approved implementation after discussing choices. No selector, image
management, network isolation or host firewall mutation is in scope. Native tmux
clients execute inside rail via fixed list/attach/resize IPC and passed terminal
FDs; host launchers never execute tmux peer commands. Final config symlinks are
rejected to protect named entrypoints. Docker path/parent allows require its
enabled authorized service. TTC_DATA_DIR preserves the selected shared data root.
Independent review findings were fixed and the follow-up found no critical
issues. The configuration skill warns that outside-workspace edits are not
undoable. Shared networking and authorized Docker access remain deliberate
limitations, not complete host security isolation.
