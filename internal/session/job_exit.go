package session

import (
	"encoding/json"

	"ttc/internal/capture"
	"ttc/internal/jobs"
)

// jobExitContent carries the immutable completion selection rather than forcing
// a second read for modest results. Labels and timestamps stay in job metadata.
func jobExitContent(v jobs.Snapshot) string {
	var stderr *string
	if v.Kind == "subagent" {
		// Assignment completion is the authoritative child handoff. Even a
		// mistakenly routed generic completion must not expose diagnostics.
		v.StderrSelection = nil
		v.Truncated = v.StdoutTruncated
		if v.StdoutSelection != nil {
			v.Truncated = v.StdoutSelection.OmittedBytes > 0 || v.StdoutSelection.CleaningError != ""
		}
	} else {
		stderr = &v.Stderr
	}
	body := struct {
		Type            string             `json:"type"`
		ID              string             `json:"job_id"`
		Kind            string             `json:"kind"`
		Status          string             `json:"status"`
		Stdout          string             `json:"stdout"`
		Stderr          *string            `json:"stderr,omitempty"`
		ExitCode        *int               `json:"exit_code,omitempty"`
		Signal          string             `json:"signal,omitempty"`
		Truncated       bool               `json:"truncated"`
		StdoutSelection *capture.Selection `json:"stdout_selection,omitempty"`
		StderrSelection *capture.Selection `json:"stderr_selection,omitempty"`
	}{"job_exit", v.ID, v.Kind, v.Status, v.Stdout, stderr, v.ExitCode, v.Signal, v.Truncated, v.StdoutSelection, v.StderrSelection}
	encoded, _ := json.Marshal(body) // Contains only strings, integers and fixed structs.
	return string(encoded)
}
