package tui

import "ttc/internal/session"

type line struct {
	actor, subagentName string // Presentation attribution; never part of the model message.
	text                string
	speaker             string // Separate left-aligned label; its body is indented by two cells.
	requestID           int64  // Stable assistant-block identity, including replayed completions.
	id                  int64
	human               bool
	turnTime            string // Local HH:MM for an idle-submitted user input; empty for in-turn inputs.
	boundary            bool   // Synthetic divider/spacing row, not part of the highlighted input.
	system              bool
	styled              bool // Generated ANSI row (such as a streaming speaker label), independent of Markdown source.
	markdown            bool
	brief               bool // One clipped row; the complete saved result remains inspectable.
	compactResponse     bool // Child assistant/final response; text retains the complete source.
	callID              string
	questionID          string // Tagged question-status identity, retained when closed for inspection.
	jobID               string
	detail              string
	complete            bool
	awaiting            bool // Tool name announced; arguments are still being streamed.
	image               *session.ImageSnapshot
	assets              []placedImage
}
type input struct{ text string }
type operationResult struct {
	text     string
	command  string // Slash command name; empty for a model turn or editor result.
	markdown bool
	err      error
}
