package tool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"ttc/internal/prompts"

	"ttc/internal/jobs"
	"ttc/internal/lsp"
	"ttc/internal/workspace"
)

type lspArgs struct {
	JobID      string  `json:"job_id"`
	Operation  string  `json:"operation"`
	Path       *string `json:"path,omitempty"`
	LanguageID *string `json:"language_id,omitempty"`
	Line       *int    `json:"line,omitempty"`
	Column     *int    `json:"column,omitempty"`
	Query      *string `json:"query,omitempty"`
	Offset     *int    `json:"offset,omitempty"`
	Limit      *int    `json:"limit,omitempty"`
	Timeout    *int    `json:"timeout_ms,omitempty"`
}

func validateLSP(a lspArgs) error {
	if err := Required("job_id", a.JobID); err != nil {
		return err
	}
	switch a.Operation {
	case "definition", "references", "hover", "document_symbols":
		if a.Path == nil || strings.TrimSpace(*a.Path) == "" {
			return errors.New("path is required for this operation")
		}
		if a.Query != nil {
			return errors.New("query is only accepted by workspace_symbols; omit it")
		}
		if a.Operation == "document_symbols" {
			if a.Line != nil || a.Column != nil {
				return errors.New("document_symbols does not take line or column; omit them")
			}
		} else if a.Line == nil || a.Column == nil || *a.Line < 1 || *a.Column < 1 {
			return errors.New("line and column are required positive 1-based Unicode code-point positions")
		}
	case "workspace_symbols":
		if a.Query == nil {
			return errors.New("workspace_symbols requires query (an empty string is allowed)")
		}
		if a.Path != nil || a.LanguageID != nil || a.Line != nil || a.Column != nil {
			return errors.New("workspace_symbols only accepts query and pagination; omit path, language_id, line and column")
		}
	default:
		return errors.New("operation must be definition, references, hover, document_symbols or workspace_symbols")
	}
	if a.LanguageID != nil && (strings.TrimSpace(*a.LanguageID) == "" || strings.ContainsAny(*a.LanguageID, "\x00\r\n")) {
		return errors.New("language_id must be a nonempty language identifier")
	}
	if a.Operation == "hover" && (a.Offset != nil || a.Limit != nil) {
		return errors.New("hover is not paginated; omit offset and limit")
	}
	if a.Offset != nil && *a.Offset < 0 {
		return errors.New("offset must be nonnegative")
	}
	if err := rangeInt("limit", a.Limit, 1, 500); err != nil {
		return err
	}
	return rangeInt("timeout_ms", a.Timeout, 1, 120000)
}

func jobToolError(err error) error {
	if errors.Is(err, jobs.ErrNotFound) {
		return Fail("not_found", err.Error())
	}
	var failure *lsp.Error
	if errors.As(err, &failure) {
		return Fail(failure.Code, failure.Message)
	}
	return err
}

func addLSP(r *Registry, m *jobs.Manager, w *workspace.Manager) {
	Register(r, "lsp_query", prompts.ToolDescription("lsp_query"), map[string]any{
		"job_id": Property("string"), "operation": Property("string", "definition", "references", "hover", "document_symbols", "workspace_symbols"), "path": Property("string"), "language_id": Property("string"), "line": Property("integer"), "column": Property("integer"), "query": Property("string"), "offset": Property("integer"), "limit": Property("integer"), "timeout_ms": Property("integer"),
	}, []string{"job_id", "operation"}, validateLSP, func(ctx context.Context, x Execution, a lspArgs) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, time.Duration(intDefault(a.Timeout, 30000))*time.Millisecond)
		defer cancel()
		query := lsp.Query{Operation: a.Operation, Offset: intDefault(a.Offset, 0), Limit: intDefault(a.Limit, 100)}
		if a.Path != nil {
			query.Path = w.Path(*a.Path)
		}
		if a.LanguageID != nil {
			query.LanguageID = *a.LanguageID
		}
		if a.Line != nil {
			query.Line = *a.Line
		}
		if a.Column != nil {
			query.Column = *a.Column
		}
		if a.Query != nil {
			query.Text = *a.Query
		}
		result, err := m.QueryLSP(ctx, x.Actor, a.JobID, query)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
			return nil, Fail("timeout", fmt.Sprintf("LSP query exceeded %d ms; increase timeout_ms (max 120000), narrow query, or inspect server stderr", intDefault(a.Timeout, 30000)))
		}
		return result, jobToolError(err)
	})
}
