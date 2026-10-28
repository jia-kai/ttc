package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// BoundaryCapture identifies a captured native event within a request attempt.
// ID can be passed to BoundaryRawPage to retrieve that event's exact bytes.
type BoundaryCapture struct {
	ID                             int64
	Direction                      string
	Label                          string
	readable, expanded, prettyJSON string
	structured                     bool
	deltaEmpty                     bool
}

func (s *Store) boundaryTarget(ctx context.Context, id int64) (boundaryRecord, error) {
	var encoded []byte
	if err := s.db.QueryRowContext(ctx, `SELECT content_json FROM entries WHERE id=? AND kind='status' AND json_extract(content_json,'$.type')='presentation_stream'`, id).Scan(&encoded); err != nil {
		return boundaryRecord{}, err
	}
	var record boundaryRecord
	err := json.Unmarshal(encoded, &record)
	return record, err
}

// BoundaryCaptures lists request, responses and outcome through the supplied
// immutable revision, in capture order. A header ID selects the request-only
// snapshot; use the latest Item.Revision for its current grouped view.
func (s *Store) BoundaryCaptures(ctx context.Context, id int64) ([]BoundaryCapture, error) {
	record, err := s.boundaryTarget(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.boundaryCapturesAt(ctx, record, id)
}

// Keep the bounded ancestry outermost. An ordinary JOIN lets SQLite scan every
// entry in ID order to avoid a small sort, making one inspection depend on the
// entire database size rather than the selected attempt.
const boundaryCapturesQuery = boundaryAttemptAncestry + `SELECT e.id,e.content_json
 FROM ancestry a CROSS JOIN entries e ON e.id=a.id
 WHERE e.kind='status' AND json_extract(e.content_json,'$.type')='presentation_stream'
 AND json_extract(e.content_json,'$.request')=? AND json_extract(e.content_json,'$.attempt')=?
 AND json_extract(e.content_json,'$.direction') IN ('request','response','outcome') ORDER BY e.id`

func (s *Store) boundaryCapturesAt(ctx context.Context, record boundaryRecord, tip int64) ([]BoundaryCapture, error) {
	rows, err := s.db.QueryContext(ctx, boundaryCapturesQuery, record.Request, record.Attempt, 0, tip, record.Request, record.Attempt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var details []BoundaryCapture
	response := 0
	for rows.Next() {
		var entryID int64
		var raw []byte
		if err := rows.Scan(&entryID, &raw); err != nil {
			return nil, err
		}
		var r boundaryRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		label := r.Direction
		if r.Direction == "response" {
			response++
			label = fmt.Sprintf("Response #%d", response)
		}
		details = append(details, BoundaryCapture{ID: entryID, Direction: r.Direction, Label: label, readable: r.Readable, expanded: r.Expanded, prettyJSON: r.JSON, structured: r.ReadableVersion > 0, deltaEmpty: r.DeltaEmpty})
	}
	return details, rows.Err()
}

func (d BoundaryCapture) heading() string {
	// Typed outcome documents already carry an Outcome section. Legacy artifacts
	// and other captures need an outer heading identifying their place in the group.
	if d.Direction == "outcome" && d.structured {
		return ""
	}
	if d.Direction == "response" {
		return d.Label + "\n"
	}
	return strings.ToUpper(d.Direction[:1]) + d.Direction[1:] + "\n"
}
func (d BoundaryCapture) path(expanded bool) string {
	if expanded && d.expanded != "" {
		return d.expanded
	}
	return d.readable
}

type boundaryReadableSource struct {
	text, path string
	json       bool // Optional appended display; never displaces the human-readable view.
}

// All readable projections precede every native JSON document. Source labels
// live outside JSON; captured objects never acquire synthetic grouping fields.
func boundaryReadableSources(details []BoundaryCapture, expanded bool) []boundaryReadableSource {
	sources := make([]boundaryReadableSource, 0, len(details)*2+1)
	for i, d := range details {
		heading := d.heading()
		if i > 0 {
			heading = "\n\n" + heading
		}
		sources = append(sources, boundaryReadableSource{text: heading, path: d.path(expanded)})
	}
	for _, d := range details {
		if d.deltaEmpty {
			continue
		}
		label := strings.ToUpper(d.Direction[:1]) + d.Direction[1:]
		if d.Direction == "response" {
			label = d.Label
		}
		heading := "\n\n" + label + "\n"
		if d.prettyJSON == "" {
			heading += "Prettified JSON unavailable for this older capture. Exact original bytes remain available in the raw view."
		}
		sources = append(sources, boundaryReadableSource{text: heading, path: d.prettyJSON, json: true})
	}
	return sources
}

type boundaryLayoutKey struct {
	id       int64
	expanded bool
}
type boundaryLayoutPart struct {
	start, end int    // Unicode character offsets in the complete virtual document.
	text       string // Small group headings/separators only, never capture bodies.
	chunk      boundaryChunk
	root       string
}
type boundaryLayout struct {
	parts []boundaryLayoutPart
	chars int
}
type boundaryLayoutCache struct {
	mu      sync.Mutex
	entries map[boundaryLayoutKey]*boundaryLayout
	order   []boundaryLayoutKey
	parts   int
}

// Layouts contain authenticated immutable indexes, not payload buffers. Bound
// both entry count and aggregate chunk metadata so many long requests cannot
// retain unbounded manifests. Filesystem/body verification still occurs on reads.
func (c *boundaryLayoutCache) get(key boundaryLayoutKey) *boundaryLayout {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[key]
}
func (c *boundaryLayoutCache) put(key boundaryLayoutKey, layout *boundaryLayout) {
	const maxLayouts = 32
	const maxParts = 65536
	if len(layout.parts) > maxParts {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[boundaryLayoutKey]*boundaryLayout)
	}
	if c.entries[key] != nil {
		return
	}
	for len(c.order) > 0 && (len(c.order) >= maxLayouts || c.parts+len(layout.parts) > maxParts) {
		old := c.order[0]
		c.order = c.order[1:]
		c.parts -= len(c.entries[old].parts)
		delete(c.entries, old)
	}
	c.entries[key] = layout
	c.order = append(c.order, key)
	c.parts += len(layout.parts)
}
func (s *Store) boundaryGroupLayout(ctx context.Context, id int64, expanded bool) (*boundaryLayout, error) {
	key := boundaryLayoutKey{id: id, expanded: expanded}
	if layout := s.boundaryLayouts.get(key); layout != nil {
		return layout, ctx.Err()
	}
	record, err := s.boundaryTarget(ctx, id)
	if err != nil {
		return nil, err
	}
	details, err := s.boundaryCapturesAt(ctx, record, id)
	if err != nil {
		return nil, err
	}
	layout, err := buildBoundaryLayout(ctx, boundaryReadableSources(details, expanded), boundaryArtifactBytes)
	if err != nil {
		return nil, err
	}
	s.boundaryLayouts.put(key, layout)
	return layout, nil
}

const boundaryJSONOmission = "\n\nRemaining prettified JSON omitted to preserve the readable view within the display limit. Press R for exact raw captures."

// Human content owns the display budget. Optional JSON documents are included
// whole or omitted, never truncated into invalid JSON or allowed to hide the
// readable view. A fixed section heading/omission notice may exceed the budget
// when human content already fills it; no capture body receives that exemption.
func buildBoundaryLayout(ctx context.Context, sources []boundaryReadableSource, budget int64) (*boundaryLayout, error) {
	layout := &boundaryLayout{}
	bytes := int64(0)
	addText := func(text string) {
		if text == "" {
			return
		}
		n := len([]rune(text))
		layout.parts = append(layout.parts, boundaryLayoutPart{start: layout.chars, end: layout.chars + n, text: text})
		layout.chars += n
		bytes += int64(len(text))
	}
	jsonStarted := false
	for i, source := range sources {
		var reader *boundaryChunkReader
		cost := int64(len(source.text))
		if source.path != "" {
			var err error
			reader, err = openBoundaryRaw(ctx, source.path)
			if err != nil {
				return nil, err
			}
			cost += int64(reader.manifest.Bytes)
		}
		if source.json {
			if !jsonStarted {
				addText("\n\nPrettified raw JSON")
				jsonStarted = true
			}
			reserve := int64(len(boundaryJSONOmission))
			if i == len(sources)-1 {
				reserve = 0
			}
			if bytes+cost+reserve > budget {
				addText(boundaryJSONOmission)
				break
			}
		} else if bytes+cost > budget {
			return nil, errors.New("grouped readable boundary exceeds display limit")
		}
		addText(source.text)
		if reader == nil {
			continue
		}
		bytes += int64(reader.manifest.Bytes)
		// Index each chunk separately so paging is O(log(chunks)+requested chunks),
		// with no repeated scans of preceding captures or manifests.
		for _, chunk := range reader.manifest.Chunks {
			layout.parts = append(layout.parts, boundaryLayoutPart{start: layout.chars, end: layout.chars + chunk.Chars, chunk: chunk, root: reader.root})
			layout.chars += chunk.Chars
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return layout, nil
}

// boundaryGroupPage pages compact readable capture artifacts. Only requested
// chunks are loaded and authenticated, independently of cached layout metadata.
func (s *Store) boundaryGroupPage(ctx context.Context, id int64, offset, limit int) (string, int, error) {
	return s.boundaryReadablePage(ctx, id, offset, limit, false)
}
func (s *Store) boundaryReadablePage(ctx context.Context, id int64, offset, limit int, expanded bool) (string, int, error) {
	layout, err := s.boundaryGroupLayout(ctx, id, expanded)
	if err != nil {
		return "", 0, err
	}
	text, err := layout.page(ctx, offset, limit)
	return text, layout.chars, err
}

func (layout *boundaryLayout) page(ctx context.Context, offset, limit int) (string, error) {
	var out strings.Builder
	first := sort.Search(len(layout.parts), func(i int) bool { return layout.parts[i].end > offset })
	for i := first; i < len(layout.parts) && layout.parts[i].start < offset+limit; i++ {
		part := layout.parts[i]
		start := max(0, offset-part.start)
		end := min(part.end-part.start, offset+limit-part.start)
		if part.text != "" {
			out.WriteString(string([]rune(part.text)[start:end]))
			continue
		}
		data, err := readBoundaryChunk(ctx, part.root, part.chunk)
		if err != nil {
			return "", err
		}
		out.WriteString(string([]rune(string(data))[start:end]))
	}
	return out.String(), ctx.Err()
}

// BoundaryExpandedPage pages the complete readable structure of the selected
// request-attempt snapshot followed by separately labeled, prettified native
// JSON documents. Old captures lacking expanded/JSON artifacts use their saved
// readable artifact and an explicit unavailable-JSON notice.
func (s *Store) BoundaryExpandedPage(ctx context.Context, id int64, offset, limit int) (InspectionPage, error) {
	page := InspectionPage{ID: id, Offset: offset, Limit: limit, Supported: true, LargeEnvelope: true, Kind: "status", Type: "presentation_stream"}
	if id <= 0 || offset < 0 || limit < 1 || limit > InspectionPageChars {
		return page, errors.New("invalid expanded boundary page")
	}
	if err := s.db.QueryRowContext(ctx, `SELECT actor_id,json_extract(content_json,'$.item.title'),coalesce(json_extract(content_json,'$.readable_version'),0)>0 FROM entries WHERE id=? AND kind='status' AND json_extract(content_json,'$.type')='presentation_stream'`, id).Scan(&page.Actor, &page.Title, &page.Structured); err != nil {
		return page, err
	}
	text, total, err := s.boundaryReadablePage(ctx, id, offset, limit, true)
	page.Text, page.Total = text, total
	return page, err
}

func (s *Store) boundaryGroupAll(ctx context.Context, id int64) (string, error) {
	layout, err := s.boundaryGroupLayout(ctx, id, false)
	if err != nil {
		return "", err
	}
	return layout.all(ctx)
}

func (layout *boundaryLayout) all(ctx context.Context) (string, error) {
	var out strings.Builder
	for _, part := range layout.parts {
		if part.text != "" {
			out.WriteString(part.text)
			continue
		}
		data, err := readBoundaryChunk(ctx, part.root, part.chunk)
		if err != nil {
			return "", err
		}
		out.Write(data)
	}
	return out.String(), ctx.Err()
}
