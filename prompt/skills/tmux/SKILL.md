---
name: tmux
description: Inspect and control tmux panes and windows, run interactive commands, and monitor long-running work on headless Linux hosts.
allowed-tools: shell wakeup_schedule wakeup_cancel job_read job_stop
---

# tmux

Use `shell` for tmux commands. TTC's shell is noninteractive and has no PTY;
send interactive input to a tmux pane instead. Check `command -v tmux` first.
If it is missing, report the dependency rather than installing it for inspection.

## Find the target

Identify TTC's own pane before sending keys. Prefer stable pane IDs (`%75`) and
window IDs (`@19`); indexes and names can change or be ambiguous. A target may
also be `session:window.pane`. Use `$TMUX_PANE` for TTC's own pane; an untargeted
`display-message` identifies the active pane, which may contain another program.
If `TMUX_PANE` is absent, inspect the pane list; do not assume the active pane
contains TTC.

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
{"command":"tmux capture-pane -t %75 -p -S -100"}
```

Shell results show a bounded tail. If more text is needed, use the returned
`job_id` with `job_read`, or save the capture to a file in the runtime
`scratch_directory`. Use `limit_bytes` up to 65536 and follow `next_cursor`:

```json
{"job_id":"job_xyz","stream":"stdout","cursor":"eof:-100:lines","limit_bytes":16384}
```

Replace example IDs with the actual target and returned job ID. Send command text
literally, then Enter separately. Quote text as shell data; do not interpolate
untrusted output into a command.

```sh
tmux send-keys -t %75 -l 'make test'
tmux send-keys -t %75 Enter
tmux send-keys -t %75 C-c
tmux select-window -t @19
```

Send individual keys to editors or pagers. `C-c` interrupts the pane's program;
inside TTC it exits TTC. `C-d` sends EOF to many shells, but scrolls down in TTC.
Do not interrupt, clear or close another pane merely to make its output cleaner.

## Long-running work

Run work that must outlive TTC in tmux. Record its pane ID, command, working
directory, log path and success criteria. Redirect logs to a suitable durable
project path; do not rely only on scrollback. TTC scratch is for disposable
experiments, not the only copy of important results. Use the supplied private
scratch path for temporary scripts/sockets; do not change `/tmp/ttc` permissions
or invent a shared scratch directory.

When launching a command, capture its exit code in the pane or a status file.
A success marker must only print after success, for example `make test &&
printf '\nTESTS PASSED\n'`. A missing marker or vanished PID does not prove failure
or success; inspect the logs and recorded exit status. Verify a server with a
health check or listening socket, not only a startup message.

For requested monitoring, use `wakeup_schedule` with a task-specific message and
the requested or appropriate interval. Cancel it with `wakeup_cancel` when
monitoring ends. Do not use a blocking foreground sleep as a progress timer.

An optional PID watcher should run through `shell` with `background:true` and
`wake_on_exit:true`. Verify the main process PID and its start identity, account
for PID reuse and zombies, and use a short sleep between checks. Shell `wait`
cannot wait for a process owned by another pane. Watcher completion reports
termination, not the task's exit status.

TTC jobs and timers survive compaction but end on session switch or exit; tmux
work continues. Reestablish monitoring when requested after returning. Keep
complete failure diagnostics before reproducing a problem, then fix and verify
within the user's scope. Do not stop the tmux task just because monitoring ends.
