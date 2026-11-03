package session

import (
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"ttc/internal/jobs"
)

const childInspectionPromptBytes = 4 << 10
const childInspectionSteerBytes = 2 << 10
const childInspectionTruncated = "\n[truncated]"

// childPromptInspection is an assignment-local UI cache, not conversation state.
// Its callback is consumed only by jobs.Inspect/JobDetail presentation; none of
// these labels or truncated values enter model requests or persisted messages.
// The job captures this cache directly so persistent-child reuse cannot redirect
// an old job to a later assignment. Inspection takes no runtime/admission locks.
type childPromptInspection struct {
	snapshot atomic.Pointer[childPromptSnapshot]
}

type childPromptSnapshot struct {
	initial string
	steers  [2]string // Oldest to newest, at most two accepted parent messages.
	count   int
	omitted bool
}

func newChildPromptInspection(prompt string) *childPromptInspection {
	inspection := &childPromptInspection{}
	inspection.snapshot.Store(&childPromptSnapshot{initial: childInspectionText(prompt, childInspectionPromptBytes)})
	return inspection
}

// appendSteer publishes a new immutable snapshot after guidance is accepted.
// The assignment admission lock serializes writers; inspection is lock-free.
func (p *childPromptInspection) appendSteer(message string) {
	if p == nil {
		return // Assignments without an inspectable job have no presentation cache.
	}
	next := *p.snapshot.Load()
	if next.count == len(next.steers) {
		next.steers[0] = next.steers[1]
		next.omitted = true
	} else {
		next.count++
	}
	next.steers[next.count-1] = childInspectionText(message, childInspectionSteerBytes)
	p.snapshot.Store(&next)
}

func (p *childPromptInspection) inspect(view jobs.Snapshot) jobs.InspectionSpec {
	snapshot := p.snapshot.Load()
	fields := []jobs.InspectionField{
		{Name: "Label", Value: view.Label},
		{Name: "Assignment prompt", Value: snapshot.initial, Language: "text"},
	}
	if snapshot.omitted {
		fields = append(fields, jobs.InspectionField{Name: "Parent steering history", Value: "Earlier messages omitted; showing the latest two accepted messages."})
	}
	for i := 0; i < snapshot.count; i++ {
		name := "Latest parent steering"
		if i < snapshot.count-1 {
			name = "Previous parent steering"
		}
		fields = append(fields, jobs.InspectionField{Name: name, Value: snapshot.steers[i], Language: "text"})
	}
	return jobs.InspectionSpec{
		Fields: fields,
		Streams: []jobs.InspectionStream{
			{Name: "stdout", Heading: "stdout · showing tail, up to 8 KiB", LimitBytes: 8192},
			{Name: "stderr", Heading: "stderr · showing tail, up to 8 KiB", LimitBytes: 8192},
		},
	}
}

// childInspectionText copies a bounded, UTF-8-safe prefix. The marker is inside
// the byte budget, and strings.Clone avoids retaining a large source allocation.
func childInspectionText(text string, limit int) string {
	if len(text) <= limit && utf8.ValidString(text) {
		return strings.Clone(text)
	}
	budget := limit - len(childInspectionTruncated)
	n := min(len(text), budget)
	for n > 0 && n < len(text) && !utf8.RuneStart(text[n]) {
		n--
	}
	prefix := strings.ToValidUTF8(text[:n], "\uFFFD")
	n = min(len(prefix), budget)
	for n > 0 && n < len(prefix) && !utf8.RuneStart(prefix[n]) {
		n--
	}
	return prefix[:n] + childInspectionTruncated
}
