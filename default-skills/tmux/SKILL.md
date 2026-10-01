---
name: tmux
description: Manipulate tmux panes and windows — view content of other panes (screenshots), send keys/commands to other panes, navigate between windows (tabs), and manage long-running processes. Use when the user asks to interact with tmux, check on a running process in another pane, send commands to another terminal, or monitor output.
allowed-tools: shell wakeup_schedule wakeup_cancel job_read job_stop
---

# Tmux Pane and Window Manipulation

You can interact with other tmux panes and windows to view their content, send commands, and manage long-running processes. All commands use the `shell` tool.

## Targeting

Tmux targets identify a specific pane or window. There are several formats:

| Format                           | Example   | Notes                                                |
| -------------------------------- | --------- | ---------------------------------------------------- |
| `session:window.pane` (by index) | `0:1.0`   | Indexes can shift when windows/panes are reordered   |
| Window unique ID                 | `@19`     | Stable — assigned at creation, never reused          |
| Pane unique ID                   | `%75`     | Stable — assigned at creation, never reused          |
| Window ID + pane index           | `@19.0`   | Mix stable window ID with pane index                 |
| Window by name                   | `:bash`   | Matches the window title; ambiguous if names collide |

**Prefer unique IDs (`@N` for windows, `%N` for panes) over indexes** — they remain valid even if windows or panes are reordered, closed, or renumbered.

## 1. Discover Windows and Panes

### List all windows in the current session

```bash
tmux list-windows -F '#{window_id} #{window_index}:#{window_name} #{window_panes} pane(s) #{?window_active,(active),}'
```

To list across all sessions, add `-a`:

```bash
tmux list-windows -a -F '#{session_name}: #{window_id} #{window_index}:#{window_name} #{?window_active,(active),}'
```

### List all panes across all windows in the current session

```bash
tmux list-panes -s -F '#{pane_id} #{session_name}:#{window_index}.#{pane_index} #{pane_width}x#{pane_height} #{pane_current_command} #{?pane_active,(active),}'
```

### Identify your own pane (so you don't accidentally send keys to yourself)

```bash
tmux display-message -p '#{pane_id} #{session_name}:#{window_index}.#{pane_index}'
```

## 2. Navigate Windows

### Switch to a window by unique ID, index, or name

```bash
tmux select-window -t @19     # by unique window ID (preferred)
tmux select-window -t :1      # by window index
tmux select-window -t :bash   # by window name
```

### Switch to next / previous window

```bash
tmux next-window
tmux previous-window
```

### Switch to the last (most recently active) window

```bash
tmux last-window
```

## 3. View Pane Content (Screenshots)

### Capture visible content of a pane

```bash
tmux capture-pane -t <target> -p
```

Example — capture pane `%75`:

```bash
tmux capture-pane -t %75 -p
```

### Capture with scrollback history

Capture the last N lines of scrollback (useful to see output that has scrolled off screen):

```bash
tmux capture-pane -t <target> -p -S -<N>
```

Example — capture the last 200 lines:

```bash
tmux capture-pane -t %75 -p -S -200
```

### Capture a specific line range

```bash
tmux capture-pane -t <target> -p -S <start> -E <end>
```

Line 0 is the top of the visible area. Positive numbers go further down within the visible area. Negative numbers go upward into scrollback history (e.g., `-S -50` starts 50 lines above the visible area). `-S -` means the very start of history, `-E -` means the very end.

### Capture everything in scrollback

```bash
tmux capture-pane -t <target> -p -S -
```

## 4. Send Keys to a Pane

### Send a command (with Enter to execute)

```bash
tmux send-keys -t <target> 'your command here' Enter
```

Example — run `npm test` in pane `%75`:

```bash
tmux send-keys -t %75 'npm test' Enter
```

### Send special keys

```bash
tmux send-keys -t <target> C-c        # Ctrl+C (interrupt)
tmux send-keys -t <target> C-d        # Ctrl+D (EOF)
tmux send-keys -t <target> C-z        # Ctrl+Z (suspend)
tmux send-keys -t <target> C-l        # Ctrl+L (clear screen)
tmux send-keys -t <target> Enter      # just press Enter
tmux send-keys -t <target> Escape     # Escape key
tmux send-keys -t <target> Up         # Arrow up (previous command)
tmux send-keys -t <target> q          # single key press (e.g., quit a pager)
```

### Send text without pressing Enter (for interactive prompts)

```bash
tmux send-keys -t <target> 'yes'
```

Then send Enter separately if needed:

```bash
tmux send-keys -t <target> Enter
```

### Send literal key sequences (disable key lookup)

Use `-l` to send text literally (prevents interpretation of key names):

```bash
tmux send-keys -t <target> -l 'Enter is just text here'
```

## 5. Common Patterns

### Check if a process is still running in a pane

```bash
tmux list-panes -s -F '#{pane_id} #{pane_current_command}' | grep <pane_id>
```

### Kill a runaway process in another pane

```bash
tmux send-keys -t <target> C-c
```

If that doesn't work, escalate:

```bash
tmux send-keys -t <target> C-\    # SIGQUIT
```

### Run a short command and check for completion

1. Send the command:
   ```bash
   tmux send-keys -t <target> 'make build && echo "===DONE==="' Enter
   ```

2. After waiting, check the output:
   ```bash
   tmux capture-pane -t <target> -p -S -50 | grep -x '===DONE==='
   ```

The marker is only emitted on success because the example uses `&&`. Its
absence does not distinguish running from failed; inspect output and the actual
exit status before drawing a conclusion.

### Monitor a long-running task

The task runs in tmux independently of TTC. TTC progress timers and PID
watchers survive compaction but end on app exit or explicit session switch;
reestablish monitoring when asked after returning. Do not stop the tmux task
merely because its monitoring runtime ends.

1. Identify the task, its stable pane ID, and the **main process PID** (not the pane's shell PID or a transient child). If the command has not started, arrange to capture its PID when launching it; otherwise inspect the process tree for the pane with `ps` and confirm the command line. Record the task name, pane ID, PID, log/output paths, and expected success criteria. Capture diagnostics to a durable file before attempting to reproduce any failure; include the full traceback, chained exceptions, exception notes, sample metadata, and RNG state where applicable. Pane scrollback alone is not a durable record.
2. Schedule a recurring status check every **45 minutes** (2700 seconds), unless the user specifies a different interval. Use `wakeup_schedule` with `delay_seconds: 2700`, `repeat_seconds: 2700`, and a task-specific name/message. This monitoring request authorizes its progress wakeups; use `wakeup_cancel` when monitoring ends. On each wakeup inspect the pane, logs, progress, and process state; report meaningful progress or blockers. Keep the schedule active while the task runs and cancel it when the task is finished or monitoring is stopped. Do not use a blocking sleep in the assistant's foreground session as a timer.
3. Independently launch a **background shell command** that waits for the main PID to terminate, using the shell tool's background mode. For a process outside the watcher's own child tree, shell `wait <pid>` does not work. On Linux, record `/proc/<pid>/stat` field 22 (start time) at the start, then loop with a short sleep until `/proc/<pid>` disappears, the start time changes (PID reuse), or the process becomes a zombie. Report the outcome when the background command completes; also inspect the task's actual exit status from its launcher/logs when available, since disappearance alone does not prove success. If PID identity cannot be verified, resolve the correct PID before starting the watcher. The background notification is the prompt completion signal; the scheduled checks are for progress and stalled/error detection.
4. If the task fails or reports an error, preserve complete diagnostics before trying to reproduce it. Inspect logs, pane output, exit status, environment, and relevant code/configuration to identify the root cause. Apply a fix within the user's task scope, run relevant checks, and restart or resume only when safe; ask before destructive actions or uncertain reruns. After each confirmed fix, write or update `long_run_fix_{suffix}.md` using a short task-specific suffix (for example `long_run_fix_training_run.md`). Record symptoms, root cause, the fix, verification, and any remaining risk. Do not claim an unverified hypothesis as a fix or create an empty issue log. If the process is replaced, watch the replacement PID and update the monitoring details.

For step 3, pass the following command to the shell tool with **background mode enabled**, replacing `<main-pid>` with the verified PID. Its notification means the watched process exited or changed identity, not that the task succeeded:

```bash
bash -c '
  pid=$1
  stat=$(cat "/proc/$pid/stat") || exit 2
  rest=${stat##*) }
  read -r -a fields <<< "$rest"
  start=${fields[19]}
  while stat=$(cat "/proc/$pid/stat" 2>/dev/null); do
    rest=${stat##*) }
    read -r -a fields <<< "$rest"
    if [[ ${fields[19]} != "$start" || ${fields[0]} == Z ]]; then break; fi
    sleep 5
  done
  printf "Main process %s has terminated or changed identity; inspect its exit status and logs.\n" "$pid"
' _ <main-pid>
```

### Start a long-running server and verify it's up

```bash
tmux send-keys -t <target> 'npm run dev' Enter
```

Then after a few seconds, capture output to verify:

```bash
tmux capture-pane -t <target> -p -S -20
```

### Clear a pane before running a command (for clean captures)

```bash
tmux send-keys -t <target> C-l
tmux send-keys -t <target> 'your command' Enter
```

## 6. Important Notes

- **Always identify your own pane first** to avoid sending keys to yourself.
- **Use unique IDs** (`%N` for panes, `@N` for windows) when possible — they are stable and unambiguous.
- **Use `capture-pane -p`** (print to stdout) so the output comes back through the `shell` tool. Without `-p`, tmux writes to an internal buffer instead.
- **Scrollback is finite** — default is 2000 lines. If you need more history, the user may have configured a larger `history-limit`.
- **When sending multiline input**, send each line with a separate `send-keys` + `Enter`.
- **For interactive programs** (vim, less, top), send individual key presses rather than full command strings.
- **Use the shell tool's background mode** for PID watchers and other long waits, so the assistant remains available for scheduled checks and completion notifications.
- **`list-windows` flags**: use `-a` to list across all sessions (no `-s` flag — that's for `list-panes`).
