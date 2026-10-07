package session

import (
	"errors"
	"fmt"
	"testing"

	"ttc/internal/history"
	"ttc/internal/prompts"
)

func TestChildFailureTemplatesPreserveExactMessageBytes(t *testing.T) {
	const base = "Child assignment failed; this is partial work, not a completed result. Partial work may have side effects; inspect the workspace and complete child transcript before continuing."
	for _, inherited := range []bool{false, true} {
		finish := history.ChildFinish{Status: "failed"}
		childFailureMetadata(&finish, inherited, "/archive.md", nil)
		want := base
		if inherited {
			want += " This assignment produced no new assistant text; the returned text is from the previous assignment."
		}
		want += " Transcript: /archive.md; exact JSONL: /archive.md.jsonl"
		if finish.Warning != want || finish.Answer != "No assistant text was produced for this assignment." {
			t.Fatalf("handoff bytes changed: answer=%q warning=%q; want warning=%q", finish.Answer, finish.Warning, want)
		}
	}
	finish := history.ChildFinish{Status: "failed", Answer: "partial"}
	childFailureMetadata(&finish, false, "", errors.New("disk failure"))
	if want := base + " Complete child transcript export failed: disk failure"; finish.Warning != want || finish.Answer != "partial" {
		t.Fatalf("export failure bytes changed: %#v; want warning=%q", finish, want)
	}
	if got := finish.Warning + prompts.ChildCompletionPublicationFailure; got != base+" Complete child transcript export failed: disk failure Completion publication failed; the runtime cannot continue." {
		t.Fatalf("publication failure spacing changed: %q", got)
	}
	cause := errors.New("storage failure")
	wrapped := fmt.Errorf(prompts.ChildCompletionCommitFailure, cause)
	if !errors.Is(wrapped, cause) || wrapped.Error() != "commit child completion: storage failure" {
		t.Fatalf("completion error wrapping changed: %v", wrapped)
	}
}
