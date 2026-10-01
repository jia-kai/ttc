package tui

import "testing"

func TestPromptRecallRestoresDraftWithoutSubmitting(t *testing.T) {
	var h promptHistory
	if got := h.move("draft", -1); got != "draft" {
		t.Fatal(got)
	}
	h.add("first")
	h.add("/model")
	h.add("/model")
	if len(h.entries) != 2 {
		t.Fatal("consecutive duplicate recalled twice")
	}
	for _, step := range []struct {
		draft     string
		direction int
		want      string
	}{{"unfinished", -1, "/model"}, {"/model", -1, "first"}, {"first", -1, "first"}, {"first", 1, "/model"}, {"/model", 1, "unfinished"}, {"unfinished", 1, "unfinished"}} {
		if got := h.move(step.draft, step.direction); got != step.want {
			t.Fatal(got, step.want)
		}
	}
	h.add("next")
	if got := h.move("new draft", -1); got != "next" {
		t.Fatal(got)
	}
}
