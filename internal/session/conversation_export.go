package session

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"

	"ttc/internal/prompts"
	"ttc/internal/scratch"
	"ttc/internal/tool"
)

type conversationExportArgs struct {
	Path   string `json:"path"`
	Format string `json:"format,omitempty"`
	Scope  string `json:"scope,omitempty"`
}

// ConversationExportResult identifies an actor-scoped, frozen history export.
// Bytes is the number of bytes written to the newly created private file.
type ConversationExportResult struct {
	Path          string `json:"path"`
	Bytes         int64  `json:"bytes"`
	Actor         string `json:"actor"`
	SourceSession string `json:"source_session"`
	SourceTip     int64  `json:"source_tip"`
	Scope         string `json:"scope"`
	Format        string `json:"format"`
}

func validateConversationExport(a conversationExportArgs) error {
	if err := tool.Required("path", a.Path); err != nil {
		return err
	}
	if a.Format != "" && a.Format != "markdown" && a.Format != "jsonl" {
		return errors.New(prompts.ConversationExportFormat)
	}
	if a.Scope != "" && a.Scope != "full" && a.Scope != "before_compaction" {
		return errors.New(prompts.ConversationExportScope)
	}
	return nil
}

func (r *Runtime) addConversationExportTool() {
	tool.Register(r.Tools, "conversation_export", prompts.ToolDescription("conversation_export"), map[string]any{
		"path":   tool.Property("string"),
		"format": map[string]any{"type": "string", "enum": []string{"markdown", "jsonl"}},
		"scope":  map[string]any{"type": "string", "enum": []string{"full", "before_compaction"}},
	}, []string{"path"}, validateConversationExport, r.exportConversation)
}

func (r *Runtime) exportConversation(ctx context.Context, x tool.Execution, a conversationExportArgs) (any, error) {
	if err := validateConversationExport(a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(x.Actor) == "" {
		return nil, errors.New(prompts.ConversationExportActor)
	}
	if a.Format == "" {
		a.Format = "markdown"
	}
	if a.Scope == "" {
		a.Scope = "full"
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Main compaction may route children to a continuation while they execute.
	// Freeze the active session and its exact tip together, then release routing
	// before snapshot selection, rendering, or filesystem work.
	r.routeMu.RLock()
	session := r.Current()
	tip, err := r.Store.SessionTip(ctx, session)
	r.routeMu.RUnlock()
	if err != nil {
		return nil, err
	}
	return r.writeConversationExport(ctx, session, tip, x.Actor, a)
}

func (r *Runtime) writeConversationExport(ctx context.Context, session string, tip int64, actor string, a conversationExportArgs) (any, error) {
	// Reserve the requested output before expensive history selection. The writer
	// owns partial-file cleanup, including failures while resolving a scope cut.
	written, err := scratch.WriteExport(ctx, a.Path, func(w io.Writer) error {
		if a.Scope == "before_compaction" {
			var err error
			session, tip, err = r.Store.ActorCompactionTip(ctx, session, actor, tip)
			if err != nil {
				return err
			}
		}
		return r.Store.WriteActorTranscript(ctx, w, session, actor, tip, a.Format)
	})
	if err != nil {
		return nil, err
	}
	return ConversationExportResult{
		Path: filepath.Clean(a.Path), Bytes: written, Actor: actor,
		SourceSession: session, SourceTip: tip, Scope: a.Scope, Format: a.Format,
	}, nil
}
