package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"ttc/internal/historytest"

	contextbuild "ttc/internal/context"
	"ttc/internal/llm"
	"ttc/internal/tool"
)

func nativeAnnouncement(t *testing.T, message *llm.Message) *[]nativeReadGroup {
	t.Helper()
	if message == nil {
		t.Fatal("missing runtime context")
	}
	if strings.Contains(message.Content, "mime_type") || strings.Contains(message.Content, `"kind":"document"`) {
		t.Fatal("verbose format definitions in runtime context", message.Content)
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(message.Content), &envelope); err != nil {
		t.Fatal(err)
	}
	fields := runtimeFields(t, message, envelope.Type)
	raw, exists := fields["binary_files"]
	if !exists {
		return nil
	}
	var groups []nativeReadGroup
	if err := json.Unmarshal(raw, &groups); err != nil {
		t.Fatal(err)
	}
	if groups == nil {
		t.Fatal("capability replacement must be an array", message.Content)
	}
	return &groups
}

func TestNativeReadGroups(t *testing.T) {
	formats := []llm.BinaryFileType{
		{MIMEType: "application/rtf", Extensions: []string{".rtf", ".rtf"}, MaxBytes: 32},
		{MIMEType: "text/rtf", Extensions: []string{".rtf"}, MaxBytes: 32},
		{MIMEType: "application/pdf", Extensions: []string{".pdf"}, MaxBytes: 32},
		{MIMEType: "application/vnd.apple.iwork", Extensions: []string{".pages", ".key"}, MaxBytes: 16},
		{MIMEType: "application/vnd.apple.pages", Extensions: []string{".pages"}, MaxBytes: 8},
	}
	want := []nativeReadGroup{
		{Extensions: []string{".pages"}, MaxBytes: 8},
		{Extensions: []string{".key"}, MaxBytes: 16},
		{Extensions: []string{".pdf", ".rtf"}, MaxBytes: 32},
	}
	if got := nativeReadGroups(formats); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if got := nativeReadGroups(nil); got == nil || len(got) != 0 {
		t.Fatal("empty support must encode as []", got)
	}
}

func TestNativeReadAnnouncementLifecycle(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Observe capabilities")
	selection := r.selection
	selection.Model.Images = false
	selection.Model.BinaryFiles = pdfCapability()
	message, cursor, err := r.runtimeContext(context.Background(), "main", selection, contextCursor{})
	if err != nil || nativeAnnouncement(t, message) == nil {
		t.Fatal(message, err)
	}
	for _, state := range []string{"running", "idle"} {
		if err := r.publishChild(tool.ChildView{ID: "main/child_fixture", ChildState: state, AssignmentStatus: "running"}); err != nil {
			t.Fatal(err)
		}
		message, cursor, err = r.runtimeContext(context.Background(), "main", selection, cursor)
		if err != nil || message != nil {
			t.Fatal("routine activity emitted context", message, err)
		}
		unchanged, _, err := r.runtimeContext(context.Background(), "main", selection, cursor)
		if err != nil || unchanged != nil {
			t.Fatal("announcement omission caused spurious snapshot", unchanged, err)
		}
	}
	if err := os.WriteFile(filepath.Join(r.Workspace.Root, "AGENTS.md"), []byte("Check units.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	message, cursor, err = r.runtimeContext(context.Background(), "main", selection, cursor)
	if err != nil || nativeAnnouncement(t, message) != nil {
		t.Fatal("project update repeated capabilities", message, err)
	}
	selection.Model.BinaryFiles = append(selection.Model.BinaryFiles, selection.Model.BinaryFiles[0])
	if message, _, err := r.runtimeContext(context.Background(), "main", selection, cursor); err != nil || message != nil {
		t.Fatal("duplicate MIME catalog entry changed effective announcement", message, err)
	}
	for i := range selection.Model.BinaryFiles {
		selection.Model.BinaryFiles[i].MaxBytes = 1024
	}
	message, cursor, err = r.runtimeContext(context.Background(), "main", selection, cursor)
	if err != nil || nativeAnnouncement(t, message) == nil {
		t.Fatal("changed byte limit omitted", message, err)
	}
	selection.Model.BinaryFiles = nil
	message, cursor, err = r.runtimeContext(context.Background(), "main", selection, cursor)
	announcement := nativeAnnouncement(t, message)
	if err != nil || announcement == nil || len(*announcement) != 0 || !strings.Contains(message.Content, `"binary_files":[]`) {
		t.Fatal("native formats were not explicitly cleared", message, err)
	}
	if message, _, err := r.runtimeContext(context.Background(), "main", selection, cursor); err != nil || message != nil {
		t.Fatal("clear announcement repeated", message, err)
	}
	message, _, err = r.runtimeContext(context.Background(), "main/child_fresh", selection, contextCursor{})
	if err != nil || nativeAnnouncement(t, message) == nil {
		t.Fatal("empty first announcement omitted", message, err)
	}
}

func TestNativeReadAnnouncementFailedAdmissionRetries(t *testing.T) {
	r, _ := runtimeFixture(t, nil)
	seedRuntime(t, r, "Read original files")
	turn := admissionTurn(t, r)
	for _, initial := range []bool{true, false} {
		if !initial {
			r.selection.Model.Images = !r.selection.Model.Images
			r.selection.Model.BinaryFiles = pdfCapability()
		}
		before := r.mainContext
		if _, err := historytest.DB(t, r.Store).Exec(`CREATE TRIGGER fail_native_admission BEFORE INSERT ON model_requests BEGIN SELECT RAISE(ABORT,'forced admission failure'); END`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.admitMain(context.Background(), turn, r.selection); err == nil {
			t.Fatal("admission unexpectedly succeeded")
		}
		if !reflect.DeepEqual(before, r.mainContext) {
			t.Fatal("failed admission advanced announcement cursor")
		}
		if _, err := historytest.DB(t, r.Store).Exec("DROP TRIGGER fail_native_admission"); err != nil {
			t.Fatal(err)
		}
		_, message, err := r.admitMain(context.Background(), turn, r.selection)
		if err != nil || nativeAnnouncement(t, message) == nil {
			t.Fatal("retry omitted native formats", message, err)
		}
	}
}

func TestNativeReadCompactionReannouncesAllActors(t *testing.T) {
	for _, actor := range []string{"main", "main/child_native", "main/btw_native"} {
		for _, changed := range []bool{false, true} {
			name := actor + "/unchanged"
			if changed {
				name = actor + "/changed"
			}
			t.Run(name, func(t *testing.T) {
				r, _ := runtimeFixture(t, nil)
				compactionBudget(t, r)
				seedCompactionHistory(t, r, strings.Repeat("Earlier evidence. ", 500))
				selection := r.selection
				_, cursor, err := r.runtimeContext(context.Background(), actor, selection, contextCursor{})
				if err != nil {
					t.Fatal(err)
				}
				r.mainContext = cursor
				if changed {
					selection.Model.Images = false
					selection.Model.BinaryFiles = pdfCapability()
				}
				turn, err := r.Store.BeginChildTurn(r.Current(), actor, selection)
				if err != nil {
					t.Fatal(err)
				}
				task := childTask{actor: actor, turn: turn, selection: selection, tools: r.Tools, aside: strings.Contains(actor, "/btw_"), estimator: &contextbuild.Estimator{}}
				messages := []llm.Message{
					{Type: llm.TypeUserInput, Content: "Research", InputTimeMS: time.Now().UnixMilli()},
					{Type: llm.TypeAssistant, Content: strings.Repeat("Earlier evidence. ", 500)},
				}
				if actor != "main" {
					seedIsolatedHistory(t, r, actor, turn, messages)
				}
				// An unsuccessful handoff must leave the original cursor intact.
				r.Provider = &childProvider{stream: func(context.Context, llm.Request, func(llm.StreamEvent) error) error { return context.Canceled }}
				if actor == "main" {
					_, err = r.compactContext(context.Background(), "", selection, nil, false)
					if !reflect.DeepEqual(r.mainContext, cursor) {
						t.Fatal("failed main compaction reset cursor")
					}
				} else {
					var failed contextCursor
					_, failed, err = r.compactChild(context.Background(), task, messages, cursor, nil)
					if !reflect.DeepEqual(failed, cursor) {
						t.Fatal("failed child compaction reset cursor")
					}
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				r.Provider = &childProvider{stream: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
					return emit(llm.StreamEvent{Kind: "text", Text: "Earlier evidence summarized."})
				}}
				var result []llm.Message
				if actor == "main" {
					_, err = r.compactContext(context.Background(), "", selection, nil, false)
					cursor = r.mainContext
					if err == nil {
						result, err = r.Store.Messages(r.Current())
					}
				} else {
					result, cursor, err = r.compactChild(context.Background(), task, messages, cursor, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				if cursor.snapshot != nil {
					t.Fatal("successful compaction retained the admitted snapshot")
				}
				message, next, err := r.runtimeContext(context.Background(), actor, selection, cursor)
				if err != nil {
					t.Fatal(err)
				}
				fields := runtimeFields(t, message, "runtime_context")
				for _, key := range []string{"project", "children", "live_jobs", "live_timers"} {
					if _, ok := fields[key]; !ok {
						t.Fatal("compaction continuation omitted baseline field", key)
					}
				}
				announcement := nativeAnnouncement(t, message)
				want := nativeReadGroups(selection.Model.BinaryFileTypes())
				if err != nil || announcement == nil || !reflect.DeepEqual(*announcement, want) {
					t.Fatal("continuation did not reannounce effective capabilities", message, err)
				}
				if actor == "main" {
					input := append(append([]llm.Message(nil), result...), *message)
					estimate := estimateUsage(selection, systemTemplate, r.Tools.Definitions(), input)
					if got := r.UsageSnapshot(); got.Input != estimate.Input || got.Reserved != estimate.Reserved {
						t.Fatal("handoff budget differs from continuation announcement", got, estimate)
					}
				}
				if message, _, err := r.runtimeContext(context.Background(), actor, selection, next); err != nil || message != nil {
					t.Fatal("continuation announcement repeated", message, err)
				}
			})
		}
	}
}
