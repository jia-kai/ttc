package history

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"ttc/internal/render"
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
	Title, Text                       string
	Offset, Total, Limit              int
	Supported, Markdown, JSON, System bool
	LargeEnvelope                     bool // Avoid loading omitted image/replay data even when displayed text is short.
}

// InspectPage reads a bounded slice of saved Markdown/message text in SQLite,
// without transferring its complete tool record into Go. Instruction files are
// streamed separately, bounded to 1 MiB. Normal small inspectors retain codecs.
func (s *Store) InspectPage(ctx context.Context, id int64, offset, limit int) (InspectionPage, error) {
	page := InspectionPage{ID: id, Offset: offset, Limit: limit}
	if id <= 0 || offset < 0 || limit < 1 || limit > InspectionPageChars {
		return page, errors.New("inspection requires a positive entry ID, nonnegative character offset and limit 1–16384")
	}
	const query = `WITH source AS (
 SELECT e.actor_id actor,e.kind kind,coalesce(e.role,'') role,
  coalesce(json_extract(e.content_json,'$.type'),'') type,
  coalesce(json_extract(e.content_json,'$.path'),'') path,
  substr(coalesce(CASE WHEN e.kind='tool_result' THEN json_extract(r.markdown_json,'$.summary')
   WHEN e.kind='tool_call' THEN c.name || ' parameters'
   ELSE coalesce(json_extract(e.content_json,'$.label'),json_extract(e.content_json,'$.text'),e.role,e.kind) END,e.kind),1,192) title,
  length(CAST(e.content_json AS BLOB))>65536 large_envelope,
  CASE WHEN e.kind='tool_result' THEN json_extract(r.markdown_json,'$.detail')
   WHEN e.kind='tool_call' THEN c.call_json
   WHEN e.kind IN ('message','summary') THEN coalesce(json_extract(e.content_json,'$.content'),'') ||
    coalesce((SELECT char(10,10) || group_concat('Image snapshot: ' || substr(coalesce(json_extract(value,'$.path'),'unnamed image'),1,192),char(10))
     FROM (SELECT value FROM json_each(e.content_json,'$.images') LIMIT 16)),'')
   WHEN json_extract(e.content_json,'$.type')='job_completion' THEN json_extract(e.content_json,'$.markdown.detail')
   WHEN json_extract(e.content_json,'$.type')='request_message'
     AND json_extract(e.content_json,'$.purpose')='compaction'
     AND json_extract(e.content_json,'$.role')='assistant' THEN json_extract(e.content_json,'$.message.content')
   END body
 FROM entries e LEFT JOIN tool_records r ON r.entry_id=e.id
 LEFT JOIN tool_calls c ON e.kind='tool_call' AND c.id=json_extract(e.content_json,'$.call_id') WHERE e.id=?)
 SELECT actor,kind,role,type,path,title,body IS NOT NULL,coalesce(length(body),0),coalesce(substr(body,?+1,?),''),coalesce(json_valid(body),0),large_envelope FROM source`
	var kind, role, typ, path string
	var supported, encoded bool
	if err := s.DB.QueryRowContext(ctx, query, id, offset, limit).Scan(&page.Actor, &kind, &role, &typ, &path, &page.Title, &supported, &page.Total, &page.Text, &encoded, &page.LargeEnvelope); err != nil {
		return page, err
	}
	page.Supported = supported
	page.System = role == "system" || role == "developer" || typ == "system_prompt"
	page.JSON = encoded || kind == "tool_call"
	page.Markdown = !page.System
	page.Title = strings.Join(strings.Fields(render.Clean(page.Title)), " ")
	if typ == "system_prompt" {
		page.Title = "System prompt"
	}
	if typ == "system_prompt" {
		text, total, err := instructionPage(ctx, path, offset, limit)
		if err != nil {
			return page, err
		}
		page.Text, page.Total, page.Supported = text, total, true
	}
	return page, nil
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
