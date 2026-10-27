package render

import "strings"

// JobReference returns a bounded, control-safe plain-text kind=identity reference.
// A nonblank label takes precedence over id; kind defaults to "job". Empty
// identity returns an empty reference. Markdown callers must escape the result;
// exact IDs belong in the underlying metadata, not this concise presentation.
func JobReference(kind, label, id string) string {
	inline := func(s string) string { return strings.Join(strings.Fields(Clean(s)), " ") }
	kind, label, id = inline(kind), inline(label), inline(id)
	if kind == "" {
		kind = "job"
	}
	if label == "" {
		label = id
	}
	if label == "" {
		return ""
	}
	return clip(kind, 32) + "=" + clip(label, 160)
}
