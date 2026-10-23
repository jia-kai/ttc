package history

import (
	"bytes"
	"encoding/json"
	"errors"

	"ttc/internal/llm"
	"ttc/internal/render"
)

// PreparedBoundary owns immutable artifact references and bounded presentation
// metadata. Prepare it before acquiring session routing/publication locks.
type PreparedBoundary struct {
	direction                           string
	attempt                             int
	lineage                             string
	raw, readable, expanded, prettyJSON string
	readableVersion                     int
	state                               string
	itemRefs                            string
	items                               []preparedItem
	request                             int64
	actor, purpose                      string
	baseID                              int64
	base                                *boundaryBase
	deltaEmpty                          bool
}

// PrepareBoundary stores exact provider bytes, typed readable projections and
// generically indented JSON. History never interprets provider-native fields.
func (s *Store) PrepareBoundary(session string, request int64, event llm.BoundaryEvent) (PreparedBoundary, error) {
	var p PreparedBoundary
	if event.Direction != "request" && event.Direction != "response" && event.Direction != "outcome" {
		return p, errors.New("invalid boundary direction")
	}
	if event.Attempt < 1 {
		return p, errors.New("boundary attempt must be positive")
	}
	if len(event.Payload) == 0 || int64(len(event.Payload)) > boundaryArtifactBytes {
		return p, errors.New("boundary requires a nonempty capture no larger than 256 MiB")
	}
	saved, err := s.Session(session)
	if err != nil {
		return p, err
	}
	p.direction, p.attempt, p.lineage = event.Direction, event.Attempt, saved.LineageID
	p.request = request
	if err := s.db.QueryRow("SELECT actor_id,purpose FROM model_requests WHERE id=?", request).Scan(&p.actor, &p.purpose); err != nil {
		return p, err
	}
	text := "Readable inspection unavailable; see exact raw capture."
	var document llm.InspectionDocument
	if event.Inspection != nil {
		document = *event.Inspection
		if event.Direction == "response" && len(document.Sections) == 1 && document.Sections[0].Title == "Response" {
			// Grouped inspection already supplies the numbered response heading.
			// Copy the slice so presentation never mutates provider-owned metadata.
			document.Sections = append([]llm.InspectionSection(nil), document.Sections...)
			document.Sections[0].Title = ""
		}
		p.state = event.Inspection.State
		if p.state != "" && (event.Direction != "outcome" || p.state != "completed" && p.state != "failed" && p.state != "interrupted") {
			return p, errors.New("invalid inspection outcome state")
		}
		p.readableVersion = render.InspectionVersion
		if event.Direction != "request" || document.Items == nil {
			text = render.Inspection(document)
		}
	}
	p.raw, err = s.boundaryArtifact(session, event.Payload)
	if err != nil {
		return p, err
	}
	if event.Direction == "request" && event.Inspection != nil && event.Inspection.Items != nil {
		if err = s.prepareBoundaryItems(event.Inspection.Items, &p); err != nil {
			return p, err
		}
		// Refresh the selected tip after expensive item preparation. Commit checks
		// the candidate base again; no filesystem reads or diff work occur there.
		var tip int64
		if err = s.db.QueryRow("SELECT coalesce(active_entry_id,0) FROM sessions WHERE id=?", session).Scan(&tip); err != nil {
			return p, err
		}
		err = s.prepareRequestDelta(tip, &p)
		p.items = nil
		return p, err
	}
	if event.Direction == "request" {
		text = "Baseline reset: item references unavailable for this capture.\n" + text
	}
	p.readable, err = s.boundaryArtifact(session, []byte(text))
	if err != nil {
		return p, err
	}
	if event.Inspection != nil {
		p.expanded, err = s.boundaryArtifact(session, []byte(render.InspectionExpanded(document)))
		if err != nil {
			return p, err
		}
	}
	p.prettyJSON, err = s.boundaryArtifact(session, prettyBoundaryJSON(event.Payload, boundaryArtifactBytes))
	return p, err
}

// prettyBoundaryJSON formats generic JSON without interpreting native fields.
// Formatting failure is display-only: malformed captures remain retained and
// must not turn a completed provider attempt into a retryable storage error.
func prettyBoundaryJSON(raw []byte, limit int64) []byte {
	const oversized = "Prettified JSON unavailable: formatted display exceeds the size limit. Exact original capture remains available in the raw view."
	// Bound indentation expansion before json.Indent allocates it. Counting all
	// input bytes plus possible inserted whitespace is conservative (existing
	// whitespace and empty containers may be counted twice), never an underestimate.
	// String contents are skipped; this scan does not extract provider fields.
	bound := int64(len(raw))
	if bound > limit {
		return []byte(oversized)
	}
	depth := int64(0)
	quoted, escaped := false, false
	for _, c := range raw {
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case '{', '[':
			depth++
			bound += 1 + 2*depth
		case '}', ']':
			if depth > 0 {
				depth--
			}
			bound += 1 + 2*depth
		case ',':
			bound += 1 + 2*depth
		case ':':
			bound++
		}
		if bound > limit {
			return []byte(oversized)
		}
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return []byte("Prettified JSON unavailable: original capture is not valid JSON. Exact original bytes remain available in the raw view.")
	}
	if int64(out.Len()) > limit {
		return []byte(oversized)
	}
	return out.Bytes()
}
