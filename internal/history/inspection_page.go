package history

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"ttc/internal/llm"
)

// InspectionPageChars is the maximum Unicode characters returned per page;
// valid UTF-8 therefore occupies at most 64 KiB before presentation framing.
const InspectionPageChars = 16384

// InspectionPage contains bounded retained text and display metadata. Offset,
// Total and Limit count Unicode characters, not bytes or wrapped terminal rows.
// Supported is false for entries requiring the normal small inspector codec.
type InspectionPage struct {
	ID                                int64
	Actor                             string // Owning actor; the frontend supplies its display badge.
	Kind, Type                        string // Entry kind and optional status discriminator, for consistent inspector attribution.
	Title, Text                       string
	Offset, Total, Limit              int
	Supported, Markdown, JSON, System bool
	Structured                        bool // Readable provider inspection generated from typed IR.
	LargeEnvelope                     bool // Avoid loading omitted attachment/replay data even when displayed text is short.
}

// Match canonical decoding without transferring the envelope into Go. String
// fields take the last non-null, case-folded value. Message.UnmarshalJSON resets
// its receiver for each object, so duplicate outer message objects do not merge.
// Go's Unicode key folding also maps long s to ASCII s in message/input_source.
const inspectionModelSource = `WITH inspection_entry AS (
 SELECT *,coalesce((SELECT value FROM json_each(entries.content_json) AS field
  WHERE lower(field.key)='type' AND field.type='text' ORDER BY field.id DESC LIMIT 1),'') canonical_type
 FROM entries WHERE id=?
), inspection_message AS (
 SELECT CASE WHEN kind IN ('message','summary') THEN content_json
  WHEN kind='status' AND canonical_type='request_message' THEN (
   SELECT CASE WHEN field.type='object' THEN field.value END FROM json_each(content_json) AS field
   WHERE lower(replace(field.key,'ſ','s'))='message' ORDER BY field.id DESC LIMIT 1)
  END content_json FROM inspection_entry
)`

// InspectPage reads a bounded slice of saved Markdown/message text in SQLite,
// without transferring its complete tool record into Go. Model-text projections
// read complete message content, but omit attachment/replay metadata. Instruction
// files are streamed separately, bounded to 1 MiB. Small inspectors retain codecs.
func (s *Store) InspectPage(ctx context.Context, id int64, offset, limit int) (InspectionPage, error) {
	page := InspectionPage{ID: id, Offset: offset, Limit: limit}
	if id <= 0 || offset < 0 || limit < 1 || limit > InspectionPageChars {
		return page, errors.New("inspection requires a positive entry ID, nonnegative character offset and limit 1–16384")
	}
	const query = inspectionModelSource + `, source AS (
 SELECT e.actor_id actor,e.kind kind,coalesce(e.role,'') role,
  e.canonical_type type,
  coalesce(json_extract(e.content_json,'$.path'),'') path,
  coalesce((SELECT value FROM json_each((SELECT content_json FROM inspection_message)) AS field
   WHERE lower(field.key)='type' AND field.type='text' ORDER BY field.id DESC LIMIT 1),'') message_type,
  coalesce((SELECT value FROM json_each((SELECT content_json FROM inspection_message)) AS field
   WHERE lower(replace(field.key,'ſ','s'))='input_source' AND field.type='text' ORDER BY field.id DESC LIMIT 1),'') input_source,
  substr(coalesce(CASE WHEN e.kind='tool_result' THEN json_extract(r.markdown_json,'$.summary')
   WHEN e.kind='tool_call' THEN c.name || ' parameters'
   ELSE coalesce(json_extract(e.content_json,'$.label'),json_extract(e.content_json,'$.text'),e.role,e.kind) END,e.kind),1,192) title,
  length(CAST(e.content_json AS BLOB))>65536 large_envelope,
  CASE WHEN e.kind='tool_result' THEN json_extract(r.markdown_json,'$.detail')
   WHEN e.kind='tool_call' THEN c.call_json
   WHEN e.kind IN ('message','summary') THEN coalesce(json_extract(e.content_json,'$.content'),'') ||
    coalesce((SELECT char(10,10) || group_concat('Binary snapshot: ' || substr(coalesce(json_extract(value,'$.path'),'unnamed file'),1,192),char(10))
     FROM (SELECT value FROM json_each(e.content_json,'$.files') LIMIT 16)),'')
   WHEN json_extract(e.content_json,'$.type')='job_completion' THEN json_extract(e.content_json,'$.markdown.detail')
   WHEN json_extract(e.content_json,'$.type')='request_message'
     AND json_extract(e.content_json,'$.purpose')='compaction'
     AND json_extract(e.content_json,'$.role')='assistant' THEN json_extract(e.content_json,'$.message.content')
   END body
 FROM inspection_entry e LEFT JOIN tool_records r ON r.entry_id=e.id
 LEFT JOIN tool_calls c ON e.kind='tool_call' AND c.id=json_extract(e.content_json,'$.call_id'))
 SELECT actor,kind,role,type,path,message_type,input_source,title,body IS NOT NULL,coalesce(length(body),0),coalesce(substr(body,?+1,?),''),coalesce(json_valid(body),0),large_envelope FROM source`
	var kind, role, typ, path, inputSource string
	var messageType llm.MessageType
	var supported, encoded bool
	if err := s.db.QueryRowContext(ctx, query, id, offset, limit).Scan(&page.Actor, &kind, &role, &typ, &path, &messageType, &inputSource, &page.Title, &supported, &page.Total, &page.Text, &encoded, &page.LargeEnvelope); err != nil {
		return page, err
	}
	projected := inspectModelText(messageType, inputSource)
	page.Kind, page.Type = kind, typ
	page = projectInspectionPage(page, role, supported, encoded)
	if projected {
		message := llm.Message{Type: messageType, InputSource: inputSource}
		// Structured projection needs the complete JSON payload, never a page
		// of it. Extract only content so huge files/native replay envelopes stay
		// in SQLite; canonical message semantics were checked when persisted.
		const contentQuery = inspectionModelSource + ` SELECT coalesce((
 SELECT value FROM json_each((SELECT content_json FROM inspection_message)) AS field
 WHERE lower(field.key)='content' AND field.type='text' ORDER BY field.id DESC LIMIT 1),'')`
		if err := s.db.QueryRowContext(ctx, contentQuery, id).Scan(&message.Content); err != nil {
			return page, err
		}
		if err := ctx.Err(); err != nil {
			return page, err
		}
		text, err := message.ModelContent()
		if err != nil {
			return page, err
		}
		page.Text, page.Total = inspectionTextPage(text, offset, limit)
		if err := ctx.Err(); err != nil {
			return page, err
		}
		page.Supported, page.Markdown, page.JSON = true, false, false
		page.System = contextPresentation(messageType)
	}
	if typ == "system_prompt" {
		text, total, err := instructionPage(ctx, path, offset, limit)
		if err != nil {
			return page, err
		}
		page.Text, page.Total, page.Supported = text, total, true
	}
	if typ == "presentation_stream" {
		var path string
		if err := s.db.QueryRowContext(ctx, `SELECT json_extract(content_json,'$.readable'),json_extract(content_json,'$.item.title'),coalesce(json_extract(content_json,'$.readable_version'),0)>0 FROM entries WHERE id=?`, id).Scan(&path, &page.Title, &page.Structured); err != nil {
			return page, err
		}
		text, total, err := s.boundaryGroupPage(ctx, id, offset, limit)
		if err != nil {
			return page, err
		}
		page.Text, page.Total, page.Supported, page.Markdown, page.JSON = text, total, true, false, false
		page.LargeEnvelope = true // Never fall back to loading the full artifact.
	}
	return page, nil
}

// inspectionTextPage slices projected text at Unicode character boundaries.
// Scanning the string avoids an O(text size) rune allocation for a bounded page.
func inspectionTextPage(text string, offset, limit int) (string, int) {
	start, end, total := len(text), len(text), 0
	for i := range text {
		if total == offset {
			start = i
		}
		if total >= offset && total-offset == limit {
			end = i
		}
		total++
	}
	// Do not keep the complete projection alive through a small substring.
	return strings.Clone(text[start:end]), total
}

func instructionPage(ctx context.Context, path string, offset, limit int) (string, int, error) {
	f, err := openArtifact(path, instructionSnapshotBytes)
	if err != nil {
		return "", 0, fmt.Errorf("read instruction snapshot: %w", err)
	}
	defer f.Close()
	stop := context.AfterFunc(ctx, func() { f.Close() })
	defer stop()
	reader := bufio.NewReader(io.LimitReader(f, instructionSnapshotBytes+1))
	var out strings.Builder
	count, bytes := 0, 0
	for {
		if count%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return "", 0, err
			}
		}
		r, n, err := reader.ReadRune()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", 0, err
		}
		bytes += n
		if bytes > instructionSnapshotBytes {
			return "", 0, errors.New("instruction snapshot exceeds 1 MiB")
		}
		if count >= offset && count-offset < limit {
			out.WriteRune(r)
		}
		count++
	}
	return out.String(), count, nil
}
