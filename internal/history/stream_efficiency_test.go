package history

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"ttc/internal/llm"
	"ttc/internal/providers/openai"
)

func boundaryRandomText(size int) string {
	rng := rand.New(rand.NewSource(819))
	b := make([]byte, size)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	for i := range b {
		b[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return string(b)
}

func TestBoundaryPagesSkipUnrequestedChunksAndVerifyManifest(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	raw := `{"input":["` + boundaryRandomText(2<<20) + `"],"instructions":"indexed"}`
	items := recordStream(t, s, session, turn, "main", "indexed", raw)
	var record boundaryRecord
	entry, err := s.Entry(items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(entry.Content, &record); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{record.Raw, record.Expanded} {
		reader, err := openBoundaryRaw(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		if len(reader.manifest.Chunks) < 4 {
			t.Fatal("insufficient test chunks")
		}
		first := filepath.Join(reader.root, reader.manifest.Chunks[0].Hash)
		last := filepath.Join(reader.root, reader.manifest.Chunks[len(reader.manifest.Chunks)-1].Hash)
		originalFirst, err := os.ReadFile(first)
		if err != nil {
			t.Fatal(err)
		}
		originalLast, err := os.ReadFile(last)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(first); err != nil {
			t.Fatal(err)
		}
		if first != last {
			if err := os.Remove(last); err != nil {
				t.Fatal(err)
			}
		}
		offset := reader.manifest.Chunks[0].Chars + 100
		page, total, err := boundaryTextPage(context.Background(), path, offset, 200)
		if err != nil || utf8.RuneCountInString(page) != 200 || total != reader.manifest.Chars {
			t.Fatal("page read outside requested chunks", len(page), total, err)
		}
		if err := os.WriteFile(first, originalFirst, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(last, originalLast, 0600); err != nil {
			t.Fatal(err)
		}
		// Valid JSON with reordered chunks must fail authentication before paging.
		reader.manifest.Chunks[0], reader.manifest.Chunks[1] = reader.manifest.Chunks[1], reader.manifest.Chunks[0]
		corrupted, err := json.Marshal(reader.manifest)
		if err != nil {
			t.Fatal(err)
		}
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, corrupted, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := boundaryTextPage(context.Background(), path, 0, 20); err == nil {
			t.Fatal("page accepted corrupted manifest")
		}
		if _, err := readBoundaryAll(context.Background(), path); err == nil {
			t.Fatal("full reader accepted corrupted manifest")
		}
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadableBoundaryCompositionSharesStableText(t *testing.T) {
	s, session, _, request := historyFixture(t)
	text := boundaryRandomText(2 << 20)
	prepare := func(instruction string) PreparedBoundary {
		t.Helper()
		raw := `{"instructions":"` + instruction + `","input":[{"role":"user","content":"` + text + `"}]}`
		p, err := s.PrepareBoundary(session.ID, request, llm.BoundaryEvent{Attempt: 1, Direction: "request", Payload: json.RawMessage(raw), Inspection: testInspection(raw)})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	prepare("first")
	size := func() int64 {
		t.Helper()
		var total int64
		if err := filepath.WalkDir(filepath.Join(s.Root, "lineages", session.LineageID), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				total += info.Size()
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return total
	}
	initial := size()
	for i := 0; i < 5; i++ {
		p := prepare(fmt.Sprintf("changed-%d", i))
		page, total, err := boundaryTextPage(context.Background(), p.expanded, 0, 100)
		if err != nil || len(page) != 100 || total < 2<<20 {
			t.Fatal(page, total, err)
		}
	}
	if growth := size() - initial; growth > 2<<20 {
		t.Fatalf("stable 2 MiB readable text duplicated across five requests: grew %d bytes", growth)
	}
}

func TestBoundaryIndexedUnicodePageAndPreparedLineage(t *testing.T) {
	s, session, turn, request := historyFixture(t)
	if _, err := s.RecordBoundary(session.ID, turn, "main", request, "context", llm.BoundaryEvent{Attempt: 1, Direction: "request", Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	raw := `{"text":"` + strings.Repeat("世界🙂", 60000) + `"}`
	prepared, err := s.PrepareBoundary(session.ID, request, llm.BoundaryEvent{Attempt: 1, Direction: "response", Payload: json.RawMessage(raw), Inspection: testInspection(raw)})
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.RecordPreparedBoundary(session.ID, turn, "main", request, "context", prepared)
	if err != nil {
		t.Fatal(err)
	}
	want := []rune(raw)
	page, err := s.BoundaryRawPage(context.Background(), items[0].Revision, 50000, 50)
	if err != nil || page.Text != string(want[50000:50050]) || page.Total != len(want) {
		t.Fatal(page, err)
	}
	other, otherTurn := startHistorySession(t, s, t.TempDir(), session.Model, "other")
	otherRequest, err := s.StartRequest(other.ID, otherTurn, "main", "coding", other.Model)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordPreparedBoundary(other.ID, otherTurn, "main", otherRequest, "context", prepared); err == nil {
		t.Fatal("accepted prepared artifacts from another lineage")
	}
}

func TestBoundaryCachedExpandedPagesAuthenticateRequestedChunks(t *testing.T) {
	s, session, turn, _ := historyFixture(t)
	items := recordStream(t, s, session, turn, "main", "context", `{"text":"`+boundaryRandomText(2<<20)+`"}`)
	ctx := context.Background()
	layout, err := s.boundaryGroupLayout(ctx, items[0].Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.boundaryGroupLayout(ctx, items[0].Revision, true)
	if err != nil || again != layout {
		t.Fatal("immutable layout not reused", err)
	}
	var chunks []boundaryLayoutPart
	for _, part := range layout.parts {
		if part.text == "" {
			chunks = append(chunks, part)
		}
	}
	if len(chunks) < 4 {
		t.Fatal("insufficient test chunks")
	}
	first, last := chunks[0], chunks[len(chunks)-1]
	if err := os.Remove(filepath.Join(last.root, last.chunk.Hash)); err != nil {
		t.Fatal(err)
	}
	page, err := s.BoundaryExpandedPage(ctx, items[0].Revision, first.start, 100)
	if err != nil || len([]rune(page.Text)) != 100 {
		t.Fatal(page, err)
	}
	path := filepath.Join(first.root, first.chunk.Hash)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 1
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BoundaryExpandedPage(ctx, items[0].Revision, first.start, 100); err == nil {
		t.Fatal("cached layout bypassed requested chunk authentication")
	}
}

// BenchmarkPrepareBoundaryParsedItemsUnchanged16MiB measures history preparation
// for an actual provider-parsed request. Provider parsing is deliberately outside
// the timer: its cost is separate from persistence/projection reuse.
func BenchmarkPrepareBoundaryParsedItemsUnchanged16MiB(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "data"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	model := llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}
	id := NewID("session")
	turn, _, err := s.StartSession(id, b.TempDir(), model, llm.Message{Type: llm.TypeUserInput, Content: "hello"})
	if err != nil {
		b.Fatal(err)
	}
	request, err := s.StartRequest(id, turn, "main", "coding", model)
	if err != nil {
		b.Fatal(err)
	}
	raw := json.RawMessage(`{"instructions":"same","input":[{"role":"user","content":"` + strings.Repeat("x", 16<<20) + `"}]}`)
	doc, err := (&openai.Adapter{}).InspectBoundary("request", raw)
	if err != nil {
		b.Fatal(err)
	}
	if len(doc.Items) == 0 {
		b.Fatal("provider did not return items")
	}
	event := llm.BoundaryEvent{Attempt: 1, Direction: "request", Payload: raw, Inspection: &doc}
	if _, err = s.RecordBoundary(id, turn, "main", request, "key", event); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.PrepareBoundary(id, request, event); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPrepareBoundaryUnchanged16MiB measures generic captures without a
// provider item projection; ParsedItems below covers the actual request path.
func BenchmarkPrepareBoundaryUnchanged16MiB(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "data"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	model := llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}
	id := NewID("session")
	turn, _, err := s.StartSession(id, b.TempDir(), model, llm.Message{Type: llm.TypeUserInput, Content: "hello"})
	if err != nil {
		b.Fatal(err)
	}
	request, err := s.StartRequest(id, turn, "main", "coding", model)
	if err != nil {
		b.Fatal(err)
	}
	event := llm.BoundaryEvent{Attempt: 1, Direction: "request", Payload: json.RawMessage(`{"instructions":"same","input":[{"role":"user","content":"` + strings.Repeat("x", 16<<20) + `"}]}`)}
	if _, err := s.PrepareBoundary(id, request, event); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(event.Payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.PrepareBoundary(id, request, event); err != nil {
			b.Fatal(err)
		}
	}
}
