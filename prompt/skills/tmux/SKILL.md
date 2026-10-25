---
name: tmux
description:
  Inspect and control tmux panes and windows, run interactive commands, and
  monitor long-running work on headless Linux hosts.
---

# tmux

This workflow requires `shell`. If it is unavailable, report that tmux
inspection or control cannot run in this role; inspect supplied logs or propose
commands instead. With `shell`, use the commands below within the assigned
scope.

TTC's shell is noninteractive and has no PTY; send interactive input to a tmux
pane instead. Check `command -v tmux` first. If it is missing, report the
dependency rather than installing it for inspection.

## Find the target

Identify TTC's own pane before sending keys. Prefer stable pane IDs (`%75`) and
window IDs (`@19`); indexes and names can change or be ambiguous. A target may
also be `session:window.pane`. Use `$TMUX_PANE` for TTC's own pane; an
untargeted `display-message` identifies the active pane, which may contain
another program. If `TMUX_PANE` is absent, inspect the pane list; do not assume
the active pane contains TTC. Choose the intended workload pane separately. Do
not send shell command text to TTC's own pane: TTC would receive it as a prompt,
not execute it as a shell command.

```sh
if [ -n "${TMUX_PANE:-}" ]; then
    tmux display-message -p -t "$TMUX_PANE" '#{pane_id} #{session_name}:#{window_index}.#{pane_index}'
fi
tmux list-panes -a -F '#{pane_id} #{session_name}:#{window_index}.#{pane_index} #{pane_current_command}'
tmux list-windows -a -F '#{window_id} #{session_name}:#{window_index}:#{window_name}'
```

If no tmux server or current session exists, use the session/socket named by the
user. Do not create or replace a server merely to inspect it. Use `-S <socket>`
consistently when working with a custom socket.

## Inspect and send input

`capture-pane -p` returns text, not an image. Negative line numbers address
scrollback; `-S -` includes all retained history. Scrollback is finite.

```json
{ "command": "tmux capture-pane -t %75 -p -S -100" }
```

Shell results contain bounded output; consult their selection/truncation
metadata to see which portions were included. If more text is needed, use the
returned `job_id` with `job_read`, or save the capture to a file in the runtime
`scratch_directory`. Use `limit_bytes` up to 65536 and follow `next_cursor`:

```json
{
  "job_id": "job_xyz",
  "stream": "stdout",
  "cursor": "eof:-100:lines",
  "limit_bytes": 16384
}
```

Replace example IDs with the actual target and returned job ID. Send command
text literally, then Enter separately. Quote text as shell data; do not
interpolate untrusted output into a command.

```sh
tmux send-keys -t %75 -l 'make test'
tmux send-keys -t %75 Enter
tmux send-keys -t %75 C-c
tmux select-window -t @19
```

Send individual keys to editors or pagers. `C-c` interrupts the pane's program;
inside TTC it exits TTC. `C-d` sends EOF to many shells, but scrolls down in
TTC. Do not interrupt, clear or close another pane merely to make its output
cleaner.

## Long-running work

When available, use `tmux_watch_run` for work that must outlive TTC. It creates
a detached session, captures stdout/stderr and completion records, and starts a
local watcher automatically. Choose a durable record directory with an existing
parent and an absent final directory; do not add manual logging or status-file
wrappers. Keep ordinary builds and tests in `shell(background=true)` instead.
For example, start a local test web server that should remain available across
TTC sessions:

```json
{
  "command": "python3 -m http.server 8000 --bind 127.0.0.1",
  "workdir": "/path/to/project",
  "record_dir": "/path/to/project/runs/web-server-unique",
  "label": "Local test server"
}
```

Use the returned `job_id` with `job_read` for captured output. To resume
observation in a later TTC session, pass the saved `record_dir` to `watch`.
Session/pane IDs are available for direct tmux inspection or control. The
session closes when the workload exits, but records remain. Stopping a watcher
stops observation only, never the workload. Missing completion after recorder
death means unknown outcome, not success.

Keep interactive editors, debuggers and shells in ordinary tmux panes using the
inspection/input workflow above. For an existing workload without durable
records, preserve logs and check its actual exit status; a vanished PID or
missing success marker proves neither success nor failure. Verify a server with
a health check or listening socket, not only a startup message.

TTC scratch is for disposable experiments, not the only copy of important
results. Use the supplied private scratch path for temporary scripts/sockets; do
not change `/tmp/ttc` permissions or invent a shared scratch directory.

For requested monitoring, use `wakeup_schedule` when available with a
task-specific message and the requested or appropriate interval. Cancel it with
`wakeup_cancel` when monitoring ends. If timer tools are unavailable, report
that recurring monitoring was not established. Do not use a blocking foreground
sleep as a progress timer.

TTC jobs and timers survive compaction but end on session switch or exit; tmux
work continues. Reestablish monitoring when requested after returning. Keep
complete failure diagnostics before reproducing a problem, then fix and verify
within the user's scope. Do not stop the tmux task just because monitoring ends.
