package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	if data.Status.Type == "request_message" {
		return render.Clean(projectLabel(data)), nil
	}
	if entry.Kind == "message" || entry.Kind == "summary" {
		message := data.Message
		content := message.Content
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
		// A pending intent must not include a result committed outside the cut.
		return "Pending call:\n\n" + render.Tool(data.Call.Name, data.Call.Arguments, nil).Detail, nil
	}
	if entry.Kind == "tool_result" {
		return data.Markdown.ExportText(), nil
	}
	return projectInspectionText(data)
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
