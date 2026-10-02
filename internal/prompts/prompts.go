// Package prompts exposes immutable LLM-facing text generated from prompt/ assets.
// Run make prompts after editing an asset; no prompt files are read at runtime.
package prompts

import "sort"

// NamingSettings configures the bounded background session-title request.
type NamingSettings struct {
	Text           string // Instructions sent to the naming model.
	OutputTokens   int    // Maximum generated tokens, including reasoning.
	MaxAttempts    int    // Total provider attempts, including the initial request.
	TimeoutSeconds int    // Deadline for the complete naming request, in seconds.
}

// Naming returns the generated settings by value; callers cannot alter embedded assets.
func Naming() NamingSettings { return naming }

type toolPrompt struct {
	Description string
	Notes       map[string]string
}

// ToolDescription returns the instructions for a registered tool. Missing assets
// indicate a build/programmer error; the generator also checks registry coverage.
func ToolDescription(name string) string {
	text, ok := toolPrompts[name]
	if !ok {
		panic("missing tool prompt: " + name)
	}
	return text.Description
}

// ToolNote returns reusable model-facing guidance associated with a tool.
// Missing names/notes indicate a build/programmer error, never user input errors.
func ToolNote(name, note string) string {
	text, ok := toolPrompts[name].Notes[note]
	if !ok {
		panic("missing tool note: " + name + "/" + note)
	}
	return text
}

// ToolNames returns a sorted copy of the tools with generated instructions.
func ToolNames() []string {
	names := make([]string, 0, len(toolPrompts))
	for name := range toolPrompts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
