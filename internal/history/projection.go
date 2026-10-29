package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mattn/go-runewidth"

	"ttc/internal/llm"
	"ttc/internal/prompts"
	"ttc/internal/render"
)

// entryPresentation holds decoded durable values and optional acquired data.
// Its projections do not query storage, open artifacts or mutate messages.
type entryPresentation struct {
	Entry       Entry
	Message     *llm.Message
	Status      statusPresentation
	Prompt      *PromptMetadata
	ToolSummary *string
	Markdown    *render.Markdown
	Call        *callPresentation
	PromptText  string
	Dimensions  []imageDimensions // In Message.Files order; zero dimensions mean unavailable.
}

type statusPresentation struct {
	Type, Text, Purpose, Role string
	Markdown                  render.Markdown
}

type callPresentation struct {
	Name              string
	Arguments, Result json.RawMessage
}

type imageDimensions struct{ Width, Height int }

type exportChildCompletion struct {
	ChildID          string `json:"child_id"`
	AssignmentStatus string `json:"assignment_status"`
	Outcome          string `json:"outcome"`
	Answer           string `json:"answer"`
	Error            string `json:"error"`
	Warning          string `json:"warning"`
}

func (c exportChildCompletion) text(answer string) string {
	status := c.AssignmentStatus
	if status == "" {
		status = c.Outcome
	}
	var parts []string
	if status != "" {
		parts = append(parts, "Assignment status: "+render.Clean(status))
	}
	if c.Error != "" {
		parts = append(parts, "Error: "+render.Clean(c.Error))
	}
	if c.Warning != "" {
		parts = append(parts, "Warning: "+render.Clean(c.Warning))
	}
	if answer != "" {
		parts = append(parts, "Subagent final answer:\n\n"+render.Clean(answer))
	}
	return strings.Join(parts, "\n\n")
}

// decodePresentation reads only metadata needed by replay. In particular it
// skips nested request messages and job details rather than copying large bodies.
func decodePresentation(entry Entry) (entryPresentation, error) {
	data := entryPresentation{Entry: entry}
	if entry.Kind == "status" {
		var marker struct{ Type string }
		if err := json.Unmarshal(entry.Content, &marker); err != nil {
			return data, err
		}
		data.Status.Type = marker.Type
		switch marker.Type {
		case "system_prompt":
			var prompt struct {
				Path      string
				RequestID int64 `json:"request_id"`
			}
			if err := json.Unmarshal(entry.Content, &prompt); err != nil {
				return data, err
			}
			data.Prompt = &PromptMetadata{Actor: entry.Actor, Purpose: "coding", Path: prompt.Path, RequestID: prompt.RequestID}
		case "request_message":
			var request struct{ Purpose, Role string }
			if err := json.Unmarshal(entry.Content, &request); err != nil {
				return data, err
			}
			data.Status.Purpose, data.Status.Role = request.Purpose, request.Role
		case "job_completion":
			var job struct{ Markdown struct{ Summary string } }
			if err := json.Unmarshal(entry.Content, &job); err != nil {
				return data, err
			}
			data.Status.Markdown.Summary = job.Markdown.Summary
		default:
			var status struct{ Text string }
			// Other status payloads are opaque. A non-text field is not a label.
			if json.Unmarshal(entry.Content, &status) == nil {
				data.Status.Text = status.Text
			}
		}
	} else if entry.Kind == "message" || entry.Kind == "summary" || entry.Visible && entry.Kind != "tool_result" && entry.Kind != "tool_call" {
		var message llm.Message
		if err := json.Unmarshal(entry.Content, &message); err != nil {
			return data, err
		}
		data.Message = &message
	}
	return data, nil
}

// decodeInspectionBody extends decoded metadata only when full detail is needed.
// Callers never decode a nested request body merely to obtain its placeholder.
func decodeInspectionBody(data entryPresentation) (entryPresentation, error) {
	if data.Entry.Kind != "status" {
		return data, nil
	}
	switch data.Status.Type {
	case "request_message":
		var request struct{ Message llm.Message }
		if err := json.Unmarshal(data.Entry.Content, &request); err != nil {
			return data, err
		}
		if err := request.Message.Validate(); err != nil {
			return data, err
		}
		data.Message = &request.Message
	case "job_completion":
		var job struct{ Markdown render.Markdown }
		if err := json.Unmarshal(data.Entry.Content, &job); err != nil {
			return data, err
		}
		data.Status.Markdown = job.Markdown
	case "system_prompt":
		// Instruction text is acquired separately by the loader.
	default:
		if data.Entry.Visible {
			var message llm.Message
			if err := json.Unmarshal(data.Entry.Content, &message); err != nil {
				return data, err
			}
			data.Message = &message
		}
	}
	return data, nil
}

func projectView(data entryPresentation) EntryView {
	return EntryView{Entry: data.Entry, Label: projectLabel(data), Message: data.Message, StatusType: data.Status.Type, Prompt: data.Prompt}
}

func projectLabel(data entryPresentation) string {
	v := data.Entry
	if v.Kind == "tool_result" && data.ToolSummary != nil {
		return *data.ToolSummary
	}
	if v.Kind == "status" {
		x := data.Status
		if x.Type == "presentation_stream" {
			var record boundaryRecord
			if json.Unmarshal(v.Content, &record) == nil {
				return record.Item.Title
			}
		}
		if x.Type == "child_turn_finished" {
			if finish, err := readChildFinish(v.Content); err == nil {
				label := "Assignment " + finish.TurnID + " · " + render.Status(finish.Outcome)
				if finish.Error != "" {
					label += " · " + render.Clean(finish.Error)
				}
				return label
			}
		}
		if x.Type == "job_completion" {
			return x.Markdown.Summary
		}
		if x.Type == "request_message" {
			return fmt.Sprintf("%s %s · inspect", x.Purpose, x.Role)
		}
		if x.Type == "system_prompt" {
			return "System prompt · inspect"
		}
		if x.Text != "" {
			return render.Clean(x.Text)
		}
		if v.InternalEvent() {
			return render.Clean(x.Type)
		}
	}
	if (v.Visible || v.Kind == "message") && data.Message != nil {
		m := data.Message
		switch m.Type.Presentation() {
		case llm.PresentationContext:
			if m.Type == llm.TypeCompactionContext {
				return "Compaction context · inspect"
			}
			return "Runtime context · inspect"
		case llm.PresentationInstruction:
			if m.Type == llm.TypeRuntimeInstruction {
				return "Runtime instructions · inspect"
			}
			return "Instructions · inspect"
		case llm.PresentationNotification:
			return "Runtime notification · inspect"
		case llm.PresentationGuidance:
			return "Child guidance · inspect"
		}
		return m.Role() + " · " + render.Clean(m.DisplayText())
	}
	if v.Actor != "main" {
		return v.Kind
	}
	return v.Actor + " · " + v.Kind
}

func projectInspection(data entryPresentation) (Inspection, error) {
	v := data.Entry
	inspection := Inspection{
		ID: v.ID, Actor: v.Actor, Kind: v.Kind, Type: data.Status.Type,
		Title:  strings.Split(projectLabel(data), "\n")[0],
		System: v.Kind == "status" || v.Role == "system" || v.Role == "developer", Markdown: true,
	}
	if data.Status.Type == "system_prompt" {
		inspection.Markdown = false
	}
	if data.Status.Type == "presentation_stream" {
		inspection.Markdown = false
	}
	if data.Status.Type == "job_completion" {
		inspection.System = false
	}
	text, err := projectInspectionText(data)
	inspection.Text = text
	return inspection, err
}

func projectInspectionText(data entryPresentation) (string, error) {
	entry := data.Entry
	if entry.Kind == "tool_call" || entry.Kind == "tool_result" {
		if data.Markdown != nil {
			return data.Markdown.Detail, nil
		}
		return render.Tool(data.Call.Name, data.Call.Arguments, data.Call.Result).Detail, nil
	}
	if entry.Kind == "status" {
		if data.Status.Type == "presentation_stream" {
			return data.PromptText, nil
		}
		if data.Status.Type == "job_completion" {
			md := data.Status.Markdown
			if md.Revision != 1 || md.Summary == "" {
				return "", errors.New(prompts.HistoryJobCompletionPresentation)
			}
			return md.Detail, nil
		}
		if data.Status.Type == "request_message" {
			if data.Status.Purpose == "compaction" && data.Message.Type == llm.TypeAssistant {
				return render.Clean(data.Message.Content), nil
			}
			b, err := json.MarshalIndent(data.Message, "", "  ")
			return render.Fence(string(b), "json"), err
		}
		if data.Status.Type == "system_prompt" {
			return data.PromptText, nil
		}
	}
	if entry.Visible {
		message := *data.Message
		message.State = nil
		format := "text"
		if json.Valid([]byte(message.Content)) {
			format = "json"
		}
		content := render.Fence(message.Content, format)
		message.Content = ""
		b, err := json.Marshal(message)
		return content + "\n\nMessage metadata:\n" + render.Fence(string(b), "json"), err
	}
	return render.Fence(string(entry.Content), "json"), nil
}

func projectExportText(data entryPresentation) (string, error) {
	entry := data.Entry
	if entry.InternalEvent() || data.Status.Type == "system_prompt" {
		return "", nil
	}
	if data.Status.Type == "request_message" || data.Status.Type == "tool_stream" {
		return "", nil
	}
	if entry.Kind == "message" || entry.Kind == "summary" {
		message := data.Message
		if message.Type == llm.TypeRuntimeContext || message.Type == llm.TypeCompactionContext || message.Type == llm.TypeSystemInstruction || message.Type == llm.TypeDeveloperInstruction {
			return "", nil
		}
		if message.Type == llm.TypeRuntimeNotification {
			var notice struct {
				Type, Kind, Label, Status, Name, Message, Answer string
				JobID                                            string `json:"job_id"`
			}
			if json.Unmarshal([]byte(message.Content), &notice) == nil {
				if notice.Type == "child_turn_finished" {
					var completion exportChildCompletion
					if err := json.Unmarshal([]byte(message.Content), &completion); err != nil {
						return "", err
					}
					if text := completion.text(completion.Answer); text != "" {
						return text, nil
					}
				}
				parts := []string{"Runtime notification"}
				if notice.Type != "" {
					parts = append(parts, strings.ReplaceAll(notice.Type, "_", " "))
				}
				for _, field := range []string{render.JobReference(notice.Kind, notice.Label, notice.JobID), notice.Name, notice.Status, notice.Message} {
					if field != "" {
						parts = append(parts, render.Clean(field))
					}
				}
				return strings.Join(parts, " · "), nil
			}
			return "Runtime notification · " + render.Clean(message.Content), nil
		}
		if message.Type == llm.TypeChildGuidance {
			var guidance struct{ Type, Message string }
			if json.Unmarshal([]byte(message.Content), &guidance) == nil && guidance.Type == "child_message" {
				return render.Clean(guidance.Message), nil
			}
		}
		content := message.DisplayText()
		if json.Valid([]byte(content)) {
			content = render.Fence(content, "json")
		}
		text := render.Clean(content)
		for i, snapshot := range message.Files {
			path := strings.ReplaceAll(render.Clean(snapshot.Path), ">", "\\>")
			link := fmt.Sprintf("Binary file: [snapshot](<%s>)", path)
			if i < len(data.Dimensions) {
				if size := data.Dimensions[i]; size.Width > 0 && size.Height > 0 {
					link += fmt.Sprintf(" · %d×%d", size.Width, size.Height)
				}
			}
			if text != "" {
				text += "\n\n"
			}
			text += link
		}
		return text, nil
	}
	if entry.Kind == "tool_call" {
		return TranscriptCall(data.Call.Name, data.Call.Arguments), nil
	}
	if entry.Kind == "tool_result" {
		return "", nil
	}
	if entry.Kind == "status" {
		if data.Status.Type == "job_completion" && (data.Status.Markdown.Revision != 1 || data.Status.Markdown.Summary == "") {
			return "", errors.New(prompts.HistoryJobCompletionPresentation)
		}
		if data.Status.Type == "job_completion" {
			return "", nil // The summary can contain captured tool output.
		}
		if data.Status.Type == "child_message" {
			var guidance struct{ Message string }
			if err := json.Unmarshal(entry.Content, &guidance); err != nil {
				return "", err
			}
			return render.Clean(guidance.Message), nil
		}
		return render.Clean(strings.TrimSuffix(projectLabel(data), " · inspect")), nil
	}
	return "", nil
}

// TranscriptCall projects an invocation without its result. Previewed parameters
// are clipped to 80 terminal cells, independently of unpreviewed parameters.
func TranscriptCall(name string, args json.RawMessage) string {
	var values map[string]any
	if json.Unmarshal(args, &values) == nil {
		preview := make(map[string]any)
		for _, key := range []string{"command", "query", "pattern", "path", "label", "name", "job_id", "child_id", "url", "document_id"} {
			if value, ok := values[key].(string); ok {
				preview[key] = runewidth.Truncate(strings.Join(strings.Fields(render.Clean(value)), " "), 80, "…")
			}
		}
		args, _ = json.Marshal(preview)
	} else {
		args = nil
	}
	return render.Tool(runewidth.Truncate(render.Clean(name), 80, "…"), args, nil).Summary
}

// TranscriptMessage renders a message using the same projection as saved history.
// It does not open binary snapshots to obtain optional image dimensions.
func TranscriptMessage(message llm.Message) (string, error) {
	return projectExportText(entryPresentation{Entry: Entry{Kind: "message"}, Message: &message})
}

func projectInspectionPage(page InspectionPage, role string, supported, encoded bool) InspectionPage {
	page.Supported = supported
	page.System = role == "system" || role == "developer" || page.Type == "system_prompt"
	page.JSON = encoded || page.Kind == "tool_call"
	page.Markdown = !page.System
	page.Title = strings.Join(strings.Fields(render.Clean(page.Title)), " ")
	if page.Type == "system_prompt" {
		page.Title = "System prompt"
	}
	return page
}
