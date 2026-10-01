package session

// helpMarkdown is shared by plain output and the terminal's Markdown inspector.
const helpMarkdown = `# TTC help

## Conversation

- **Enter** sends or queues input; **Shift+Enter** inserts a newline.
- **Up / Down** recalls submitted prompts; **Alt+Up / Down** focuses messages.
- Click a message, or focus it and press **Tab**, to inspect it.
- **Ctrl+U / Ctrl+D** scrolls; **Ctrl+C** exits and cancels work.
- **Esc Esc** interrupts the foreground turn.

## Views and input

- **Ctrl+X F** opens fullscreen copy mode; **Esc** returns.
- **Ctrl+X S** toggles the sidebar overlay; **Ctrl+X M** selects a model.
- Type **/** for commands or **@** for attachments; **Tab** accepts a suggestion.
- **Ctrl+X E** edits the draft in VISUAL, EDITOR, or vi.
- **Left / Right**, **Ctrl+B / F**: move by character.
- **Alt+B / F**: move by word; **Ctrl+W**: delete the previous word.
- **Ctrl+A / E**: line start / end; **Ctrl+K / Y**: kill / restore text.
- Paste inserts text literally without submitting it.

## Sessions and history

- **/new** or **/clear** — start a session.
- **Ctrl+X L** or **/sessions** — select sessions by date; **/load ID** — open one.
- **/undo**, **/redo** — restore journaled file edits.
- **/export PATH** — export to a new Markdown file.
- **/compact [FOCUS]** — summarize earlier context.

Main context also compacts automatically before a request exceeds its budget.
Tool-boundary cuts keep the user instruction and last two model messages/results.

## Tools and settings

- **/btw QUESTION** — ask a parallel read-only question; answer opens a popup.
- **/model** — select model and reasoning; **/model ID [VARIANT]** — select directly.
- **/attach PATH** — snapshot a file, directory, or image.
- **/jobs**, **/timers** — inspect background work.
- **/inspect ENTRY_ID** — inspect a saved message.
- **/questions [FORM_ID]** — reopen a pending question.
- **/answer FORM_ID JSON_ARRAY** — answer in plain mode.
- **/login** — device authorization in plain mode; use ttc --login for the TUI.
- **/quit** — exit.

History-changing commands require an idle turn. Model changes during work apply
at the next tool batch boundary. Shell changes are outside undo/redo.
Session loads return directly to the conversation; other action confirmations
stay in the conversation without a popup. Command errors remain visible.
`
