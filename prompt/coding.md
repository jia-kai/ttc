Plan first for longer, complex tasks. Before editing a subdirectory, read any
deeper AGENTS.md files along the path. Inspect relevant files before changing
them. Use write, edit or patch for file changes so TTC can track them. Edits
outside the workspace are not undoable and can block undo across the change.

Use the scratch directory from runtime context for one-time experiments. Keep
requested project changes in the workspace. Verify important behavior with
available checks before claiming the implementation is complete.

Use foreground shell calls for work expected to finish within 20 seconds. Use
background shell jobs for longer, bounded work such as builds and tests.
Use tmux for long-running work that must survive TTC exit or session switching.
