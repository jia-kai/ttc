package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"ttc/internal/history"
	"ttc/internal/session"
)

// Embedding the interface makes unrelated reads fail immediately in these
// focused tests; no SQLite store or running session is needed by an inspector.
type inspectorHistory struct {
	historyReader
	page       history.InspectionPage
	pageErr    error
	inspection history.Inspection
	reads      []string
}

func (h *inspectorHistory) InspectPage(_ context.Context, id int64, offset, limit int) (history.InspectionPage, error) {
	h.reads = append(h.reads, "page")
	h.page.ID, h.page.Offset, h.page.Limit = id, offset, limit
	return h.page, h.pageErr
}

func (h *inspectorHistory) ReadInspection(_ context.Context, _ int64) (history.Inspection, error) {
	h.reads = append(h.reads, "full")
	return h.inspection, nil
}

func inspectionController(h historyReader) *frontendController {
	return &frontendController{
		frontend: &Frontend{Runtime: &session.Runtime{}, Output: io.Discard},
		ctx:      context.Background(),
		history:  h,
		view:     newTranscript(),
		names:    map[string]string{},
	}
}

func TestInspectorPagesBeforeLoadingCompleteRecord(t *testing.T) {
	for _, largeEnvelope := range []bool{false, true} {
		t.Run(map[bool]string{false: "large body", true: "small body in large envelope"}[largeEnvelope], func(t *testing.T) {
			total := history.InspectionPageChars + 1
			if largeEnvelope {
				total = 5
			}
			h := &inspectorHistory{page: history.InspectionPage{
				Actor: "main", Kind: "message", Supported: true,
				LargeEnvelope: largeEnvelope, Total: total, Text: "Done.",
			}}
			c := inspectionController(h)
			c.inspectSaved(42)
			if got := strings.Join(h.reads, ","); got != "page" {
				t.Fatalf("reads = %s; full record must not be loaded", got)
			}
			if c.modal.window == nil || c.modal.window.pages == nil {
				t.Fatal("expected bounded paged inspector")
			}
		})
	}
}

func TestInspectorUsesTypedPresentationAfterSmallPage(t *testing.T) {
	h := &inspectorHistory{
		page: history.InspectionPage{Supported: false},
		inspection: history.Inspection{
			Actor: "main", Kind: "status", Type: "system_prompt",
			Title: "Loaded instructions", Text: "Exact instruction text", System: true,
		},
	}
	c := inspectionController(h)
	c.inspectSaved(42)
	if got := strings.Join(h.reads, ","); got != "page,full" {
		t.Fatalf("reads = %s", got)
	}
	window := c.modal.window
	if window == nil || window.Title != h.inspection.Title || window.Text != h.inspection.Text || !window.System || window.Markdown || window.SourceAgent != "" {
		t.Fatalf("inspector did not preserve typed presentation: %+v", window)
	}
}

func TestInspectorDoesNotFallThroughAfterPageError(t *testing.T) {
	h := &inspectorHistory{pageErr: errors.New("page unavailable")}
	c := inspectionController(h)
	c.inspectSaved(42)
	if got := strings.Join(h.reads, ","); got != "page" {
		t.Fatalf("reads = %s", got)
	}
	if c.modal.window != nil || len(c.view.lines) != 1 || !strings.Contains(c.view.lines[0].text, "page unavailable") {
		t.Fatal("page failure was not presented without a full read")
	}
}

func TestPromptPlaceholdersUseTypedActorPurposeGroups(t *testing.T) {
	c := inspectionController(nil)
	c.seenPrompts = map[string]int64{}
	prompt := func(actor, purpose string) *history.PromptMetadata {
		return &history.PromptMetadata{Actor: actor, Purpose: purpose, Path: "snapshot"}
	}
	coding := prompt("main", "coding")
	if !c.showPromptMetadata(10, coding) {
		t.Fatal("first prompt was hidden")
	}
	c.view.append(line{id: 10, text: "System prompt · inspect"})
	if c.showPromptMetadata(20, coding) || c.view.lines[0].id != 20 {
		t.Fatal("newer prompt did not replace the group's inspection identity")
	}
	if c.showPromptMetadata(15, coding) || c.view.lines[0].id != 20 {
		t.Fatal("older prompt replaced the latest snapshot")
	}
	if !c.showPromptMetadata(30, prompt("main", "naming")) || !c.showPromptMetadata(40, prompt("main/child_one", "coding")) {
		t.Fatal("distinct actors or request purposes shared a placeholder")
	}
}
