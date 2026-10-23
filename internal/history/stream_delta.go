package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ttc/internal/llm"
	"ttc/internal/messagestream"
	"ttc/internal/render"
)

// boundaryItemRef contains provider-owned labels, hashes and immutable artifacts.
// History never extracts or interprets native request members.
type boundaryItemRef struct {
	Section  string `json:"section"`
	Name     string `json:"name"`
	Hash     string `json:"hash"`
	Readable string `json:"readable"`
	Expanded string `json:"expanded"`
	JSON     string `json:"json"`
}
type preparedItem struct {
	ref                        boundaryItemRef
	readable, expanded, native rawManifest
}
type boundaryBase struct {
	Header  int64 `json:"header"`
	Request int64 `json:"request"`
	Attempt int   `json:"attempt"`
}

func (s *Store) prepareBoundaryItems(items []llm.InspectionItem, p *PreparedBoundary) error {
	refs := make([]boundaryItemRef, 0, len(items))
	p.items = make([]preparedItem, 0, len(items))
	for _, item := range items {
		if len(item.Hash) != 64 || len(item.JSON) == 0 {
			return errors.New("inspection item requires a SHA-256 hash and native JSON")
		}
		key := boundaryItemKey{lineage: p.lineage, hash: item.Hash, section: item.Section, name: item.Name, version: render.InspectionVersion}
		if cached, ok := s.boundaryItems.get(key); ok {
			refs = append(refs, cached.ref)
			p.items = append(p.items, cached)
			continue
		}
		if !json.Valid(item.JSON) {
			return errors.New("inspection item requires a hash and valid native JSON")
		}
		doc := llm.InspectionDocument{Sections: []llm.InspectionSection{{Title: item.Section, Fields: []llm.InspectionField{{Name: item.Name, Value: item.Value}}}}}
		pi := preparedItem{ref: boundaryItemRef{Section: item.Section, Name: item.Name, Hash: item.Hash}}
		artifacts := []struct {
			data     []byte
			path     *string
			manifest *rawManifest
		}{
			{[]byte(render.Inspection(doc) + "\n\n"), &pi.ref.Readable, &pi.readable},
			{[]byte(render.InspectionExpanded(doc) + "\n\n"), &pi.ref.Expanded, &pi.expanded},
			{prettyBoundaryJSON(item.JSON, boundaryArtifactBytes), &pi.ref.JSON, &pi.native},
		}
		for _, a := range artifacts {
			path, err := s.boundaryArtifactInLineage(p.lineage, a.data)
			if err != nil {
				return err
			}
			*a.path = path
			r, err := openBoundaryRaw(context.Background(), path)
			if err != nil {
				return err
			}
			*a.manifest = r.manifest
		}
		s.boundaryItems.put(key, pi)
		refs = append(refs, pi.ref)
		p.items = append(p.items, pi)
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return err
	}
	if len(b) > 16<<20 {
		return errors.New("ordered inspection item references exceed 16 MiB")
	}
	p.itemRefs, err = s.artifactInLineage(p.lineage, "boundaryitems", b)
	return err
}

// composeBoundary joins authenticated chunk metadata only. Item formatting and
// chunk creation have already happened outside routing/publication locks.
func (s *Store) composeBoundary(lineage string, prefix string, parts []rawManifest) (string, error) {
	m := rawManifest{}
	if prefix != "" {
		path, err := s.boundaryArtifactInLineage(lineage, []byte(prefix))
		if err != nil {
			return "", err
		}
		r, err := openBoundaryRaw(context.Background(), path)
		if err != nil {
			return "", err
		}
		m = r.manifest
		m.Hash = ""
	}
	for _, part := range parts {
		m.Bytes += part.Bytes
		m.Chars += part.Chars
		m.Chunks = append(m.Chunks, part.Chunks...)
	}
	if int64(m.Bytes) > boundaryArtifactBytes {
		return "", errors.New("composed boundary exceeds display limit")
	}
	return s.saveBoundaryManifestInLineage(lineage, m)
}

// boundaryNearestBase stops recursion at the first matching header, without
// sorting or walking the prefix before that request. No admission cutoff applies.
const boundaryNearestBase = `WITH RECURSIVE scope(actor,purpose) AS (VALUES(?,?)), ancestry(id,parent_id,matched) AS (
 SELECT e.id,e.parent_id,CASE WHEN e.kind='status' AND e.actor_id=(SELECT actor FROM scope) THEN coalesce(json_extract(e.content_json,'$.type')='presentation_stream' AND json_extract(e.content_json,'$.direction')='request' AND json_extract(e.content_json,'$.purpose')=(SELECT purpose FROM scope),0) ELSE 0 END FROM entries e WHERE e.id=?
 UNION ALL SELECT e.id,e.parent_id,CASE WHEN e.kind='status' AND e.actor_id=(SELECT actor FROM scope) THEN coalesce(json_extract(e.content_json,'$.type')='presentation_stream' AND json_extract(e.content_json,'$.direction')='request' AND json_extract(e.content_json,'$.purpose')=(SELECT purpose FROM scope),0) ELSE 0 END FROM entries e JOIN ancestry a ON e.id=a.parent_id WHERE NOT a.matched) `

func (s *Store) prepareRequestDelta(tip int64, p *PreparedBoundary) error {
	var baseJSON []byte
	var base boundaryRecord
	var baseID int64
	err := s.db.QueryRow(boundaryNearestBase+`SELECT e.id,e.content_json FROM ancestry a JOIN entries e ON e.id=a.id WHERE a.matched LIMIT 1`, p.actor, p.purpose, tip).Scan(&baseID, &baseJSON)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	r := &boundaryRecord{Items: p.itemRefs}
	if err == nil {
		if err = json.Unmarshal(baseJSON, &base); err != nil {
			return err
		}
		r.Base = &boundaryBase{Header: baseID, Request: base.Request, Attempt: base.Attempt}
	}
	p.baseID = baseID
	defer func() {
		p.readable, p.expanded, p.prettyJSON = r.Readable, r.Expanded, r.JSON
		p.base, p.deltaEmpty = r.Base, r.DeltaEmpty
	}()
	r.Items = p.itemRefs
	reason := "First request baseline"
	var previous []boundaryItemRef
	if baseID != 0 {
		if base.Items == "" {
			reason = "Baseline reset: previous capture has no item references"
		} else {
			b, err := readBoundaryCAS(base.Items, 16<<20)
			if err != nil {
				return err
			}
			if err = json.Unmarshal(b, &previous); err != nil {
				return err
			}
			reason = ""
		}
	}
	oldHashes := make([]string, len(previous))
	newHashes := make([]string, len(p.items))
	for i, item := range previous {
		oldHashes[i] = item.Hash
	}
	for i, item := range p.items {
		newHashes[i] = item.ref.Hash
	}
	var matches []messagestream.Match
	if reason == "" {
		var complete bool
		matches, complete = messagestream.DiffHashes(oldHashes, newHashes)
		if !complete {
			reason = "Baseline reset: item diff work/memory budget exceeded"
		}
	}
	newSame := make([]bool, len(p.items))
	for _, m := range matches {
		newSame[m.Current] = true
	}
	var header strings.Builder
	if r.Base != nil {
		fmt.Fprintf(&header, "Base: request #%d · attempt %d\n", r.Base.Request, r.Base.Attempt)
	}
	added := len(p.items) - len(matches)
	removed := len(previous) - len(matches)
	if reason != "" {
		header.WriteString(reason + "\n")
		removed = 0
	}
	fmt.Fprintf(&header, "Items: %d added, %d removed, %d unchanged\n", added, removed, len(matches))
	if added == 0 && removed == 0 {
		header.WriteString("No item changes.\n")
	}
	if reason == "" && removed > 0 {
		fmt.Fprintf(&header, "Removed: %d items; original content remains in the base request.\n", removed)
	}
	var compact, expanded, native []rawManifest
	for i, item := range p.items {
		if !newSame[i] {
			compact = append(compact, item.readable)
			expanded = append(expanded, item.expanded)
			native = append(native, item.native)
		}
	}
	r.Readable, err = s.composeBoundary(p.lineage, header.String(), compact)
	if err != nil {
		return err
	}
	r.Expanded, err = s.composeBoundary(p.lineage, header.String(), expanded)
	if err != nil {
		return err
	}
	// Item documents remain separate native JSON values, never a synthetic full
	// request. A newline separates adjacent documents while preserving each item.
	if len(native) == 0 {
		r.JSON = ""
		r.DeltaEmpty = true
		return nil
	}
	separatorPath, err := s.boundaryArtifactInLineage(p.lineage, []byte("\n\n"))
	if err != nil {
		return err
	}
	separator, err := openBoundaryRaw(context.Background(), separatorPath)
	if err != nil {
		return err
	}
	separated := make([]rawManifest, 0, len(native)*2)
	for i, m := range native {
		if i > 0 {
			separated = append(separated, separator.manifest)
		}
		separated = append(separated, m)
	}
	nativeBytes := int64(0)
	for _, m := range separated {
		nativeBytes += int64(m.Bytes)
	}
	if nativeBytes > boundaryArtifactBytes {
		r.JSON, err = s.boundaryArtifactInLineage(p.lineage, []byte("Delta-item JSON omitted: formatted items exceed the display limit. Press R for exact raw captures."))
		return err
	}
	r.JSON, err = s.composeBoundary(p.lineage, "", separated)
	return err
}
