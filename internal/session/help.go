package session

// helpMarkdown is shared by plain output and the terminal's Markdown inspector.
const helpMarkdown = `# TTC help

Keyboard shortcuts apply to the TUI; plain-mode messages queue while busy.

## Send and stop

- **Enter / Alt+Enter while idle** — Start a normal turn.
- **Enter while busy** — Steer the active main turn after the LLM response and foreground tool batch finish; child agents are not steered.
- **Alt+Enter while busy** — Queue a new turn FIFO.
- **Foreground shells** — Delay steer delivery until they finish or move to the background.
- **/cancel-queue** — Cancel the newest queued prompt, including while waiting for turn admission, and restore its text and attachment snapshots to the composer.
- **/cancel-steer** — Do the same for the newest unadmitted steer. Both commands preserve older inputs, work busy or idle, and report empty lists.
- **Ctrl+J / Shift+Enter** — Insert a newline.
- **Paste** — Insert text without sending it.
- **Esc, Esc in the composer** — Interrupt the current turn after closing any open view.
- **Ctrl+C (any view), Ctrl+X Q, /quit** — Exit TTC and cancel live work.

## Conversation and views

- **/help** — Open this guide while busy or idle without interrupting work.
- **Up** — Recall a previous prompt, including saved prompts from older sessions.
- **Down** — Recall the next prompt or restore the draft.
- **Ctrl+R** — Search recent prompts across sessions; Enter fills input, Esc cancels.
- **Alt+Up / Alt+Down** — Focus a conversation message.
- **Click a message** — Open its details.
- **Tab with an empty input** — Inspect the focused message.
- **Ctrl+U** — Scroll up.
- **Ctrl+D** — Scroll down; at the bottom, follow new output.
- **Ctrl+X F** — Toggle fullscreen copy mode; live redraws and mouse tracking pause.
- **Ctrl+X S** — Toggle the sidebar overlay.
- **Esc in a view** — Close that view.
- **Esc while scrolled** — Return to live output.
- **/inspect ENTRY_ID** — Open a saved message's details.

## Edit input

- **Left** or **Ctrl+B without foreground shells** — Move the cursor left.
- **Right / Ctrl+F** — Move the cursor right.
- **Alt+B** — Move back one word.
- **Alt+F** — Move forward one word.
- **Ctrl+A / Home** — Move to the start of the line.
- **Ctrl+E / End** — Move to the end of the line.
- **Ctrl+W / Alt+Backspace** — Delete the previous word.
- **Alt+D** — Delete the next word.
- **Ctrl+K** — Cut text through the end of the line.
- **Ctrl+Y** — Paste the last cut text.
- **Ctrl+X E** or **/editor** — Edit the draft using VISUAL, EDITOR, or vi.
- **Ctrl+P** — Open the command menu.
- **Enter in the command menu** — Put the selected command in the input.
- **/** — Show command suggestions at the start of input.
- **@** — Show file attachment suggestions.
- **Tab in suggestions** — Accept the selected suggestion.
- **/attach PATH** — Attach a snapshot of a file, directory, or image.

## Sessions and history

These commands require an idle turn.

- **Ctrl+X N** or **/new** — Start a new session and clear all-agent run totals.
- **/clear** — Start a new session without clearing run totals.
- **Ctrl+X L** or **/sessions** — Open the session picker, grouped by date.
- **/load ID** — Load a saved session.
- **/rename TITLE** — Set a title of 1–60 characters; overrides automatic naming.
- **/undo** — Undo file-tool edits; shell changes are excluded.
- **/redo** — Restore undone file-tool edits.
- **Ctrl+X G** or **/history** — Browse user inputs; only alternate paths branch.
- **Space in history** — Inspect the selected input.
- **Enter in history** — Restore the checkpoint before the selected input.
- **/branch ENTRY_ID** — Restore a checkpoint directly.
- **/export PATH** — Write Markdown and an exact JSONL sidecar to new files.

## Context

- **/compact [FOCUS]** — Summarize earlier context while idle, optionally focusing on a topic.
- **Automatic compaction** — Summarize older context before a request exceeds its budget.
- **Compaction retention** — Preserve the last two admitted normal/queued prompts and last two committed steers, with original text/attachments and source/age markers. Recent complete model/tool cycles have a separate token cap; oversized cycles enter the summary.

## Jobs and questions

- **Ctrl+B with foreground shells** — Move all running foreground shells to the background.
- **/background** — Select a foreground shell to move to the background; plain mode moves all.
- **tmux with its default prefix** — Press Ctrl+B twice to send Ctrl+B to TTC.
- **Ctrl+X J** or **/jobs** — Inspect jobs.
- **Ctrl+X T** or **/timers** — Inspect timers.
- **/btw QUESTION** — Ask a parallel read-only agent; its answer opens in a popup.
- **Esc in a question** — Leave text editing, then dismiss without answering. The next message redirects the turn; local commands leave the question pending.
- **Ctrl+X ?** or **/questions [FORM_ID]** — Reopen the pending round with its drafts.
- **/answer FORM_ID JSON_ARRAY** — Submit question answers in plain mode.

## Model and login

- **Ctrl+X M** or **/model** — Pick a model and reasoning variant.
- **/model ID [VARIANT]** — Select a model directly.
- **Model changes during work** — Apply at the next LLM request after the current response and foreground tool batch finish.
- **/login in plain mode** — Start device authorization while idle.
- **ttc --login** — Log in before starting the TUI.
`
