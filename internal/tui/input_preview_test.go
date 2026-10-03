package tui

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/gdamore/tcell/v2"
	contextbuild "scicode/internal/context"
	"scicode/internal/provider"
	"scicode/internal/render"
)

func TestPendingInputPreviewNormalizationAndCellClipping(t *testing.T) {
	for _, tc := range []struct {
		name, source, want string
		width              int
	}{
		{"normalization", " \n\talpha\u2003 beta  \n", "Queued · alpha beta", 80},
		{"controls", "a\x1b\x00\r\v\u202e\u2066b\n\tc", "Queued · ab c", 80},
		{"invalid UTF-8", "a\xff\xfeb", "Queued · a�b", 80},
		{"combining fits", "e\u0301tail", "Queued · e\u0301", 10},
		{"ZWJ fits", "👩‍💻tail", "Queued · 👩‍💻", 11},
		{"ZWJ does not fit", "👩‍💻tail", "Queued · ", 10},
		{"wide fits", "界tail", "Queued · 界", 11},
		{"label clipped", "ignored", "Queued", 6},
		{"no cells", "ignored", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pendingInputPreview("Queued · ", tc.source, tc.width)
			if got != tc.want || !utf8.ValidString(got) || ansi.StringWidth(got) > tc.width {
				t.Fatalf("got %q (%d cells), want %q", got, ansi.StringWidth(got), tc.want)
			}
		})
	}
	// Complete short inputs use render.Clean's control safety and Fields'
	// whitespace semantics without changing authored bytes.
	source := "\x1b[31mλ\u202d\u2069\t \nword\x7f\x85tail"
	want := "Steer · " + strings.Join(strings.Fields(render.Clean(source)), " ")
	if got := pendingInputPreview("Steer · ", source, 80); got != want {
		t.Fatalf("sanitization mismatch: %q, want %q", got, want)
	}
}

func TestPendingInputPreviewOmitsSourceCutGrapheme(t *testing.T) {
	for _, source := range []string{
		strings.Repeat(" ", pendingPreviewBytes-1) + "e\u0301suffix",
		strings.Repeat(" ", pendingPreviewBytes-2) + "e\u0301suffix", // Cut inside the combining rune.
		strings.Repeat(" ", pendingPreviewBytes-len("👩‍")) + "👩‍💻suffix",
		"e" + strings.Repeat("\u0301", pendingPreviewBytes), // One oversized cluster.
		strings.Repeat(" ", pendingPreviewBytes) + "unseen suffix",
	} {
		if got := pendingInputPreview("Queued · ", source, 80); got != "Queued · " || !utf8.ValidString(got) {
			t.Fatalf("retained a partial/unseen cluster: %q", got)
		}
	}
}

func TestPendingInputPreviewBoundedWorkAndAllocation(t *testing.T) {
	short := strings.Repeat("word \t", pendingPreviewBytes/6+1)
	large := short + strings.Repeat("more words \t", 1<<17)
	want := pendingInputPreview("Queued · ", short, 80)
	if got := pendingInputPreview("Queued · ", large, 80); got != want {
		t.Fatalf("unseen suffix affected preview: %q, want %q", got, want)
	}
	measure := func(source string) testing.BenchmarkResult {
		return testing.Benchmark(func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = pendingInputPreview("Queued · ", source, 80)
			}
		})
	}
	shortResult, largeResult := measure(short), measure(large)
	if largeResult.AllocsPerOp() != shortResult.AllocsPerOp() || largeResult.AllocedBytesPerOp() != shortResult.AllocedBytesPerOp() {
		t.Fatalf("source-size-dependent allocations: short %s %s; large %s %s", shortResult, shortResult.MemString(), largeResult, largeResult.MemString())
	}
	if largeResult.AllocedBytesPerOp() > 64<<10 {
		t.Fatalf("preview allocations exceed fixed bound: %s", largeResult.MemString())
	}
	t.Logf("short: %s %s; megabyte: %s %s", shortResult, shortResult.MemString(), largeResult, largeResult.MemString())
}

func TestPendingInputDrawPreservesOriginalsAndGraphemes(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	defer s.Fini()
	s.SetSize(12, 10)
	original := "e\u0301界" + strings.Repeat(" word\n\t", 1<<17) + " original suffix  "
	input := contextbuild.Input{Text: original, Attachments: []contextbuild.Attachment{
		{Path: "snapshot.txt", Kind: "text", Text: "immutable snapshot\n", Truncated: true},
		{Path: "snapshot.png", Image: &provider.Image{DataURL: "data:image/png;base64,snapshot"}},
	}}
	before := input.Message()
	queue := []contextbuild.Input{input}
	steers := []string{"👩‍💻e\u0301suffix"}
	view, sidebar := newTranscript(), newSidebar()
	for range 3 {
		if err := draw(s, view, sidebar, false, nil, nil, -1, newComposer(""), 0, queue, steers, "", nil, provider.Selection{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.TrimRight(sidebarScreenText(s, 0, 6, 12), " "); got != "Steer · 👩‍💻e\u0301s" {
		t.Fatalf("steer grapheme draw: %q", got)
	}
	if got := sidebarScreenText(s, 0, 7, 12); got != "Queued · e\u0301界" {
		t.Fatalf("queue grapheme draw: %q", got)
	}
	if queue[0].Text != original || !reflect.DeepEqual(queue[0].Attachments, input.Attachments) || steers[0] != "👩‍💻e\u0301suffix" {
		t.Fatal("preview changed a pending original or snapshot")
	}
	if after := queue[0].Message(); !reflect.DeepEqual(after, before) {
		t.Fatal("admission message changed after redraw")
	}
	// Cancellation restores the original Input.Text, not its display preview;
	// end-to-end queue/steer cancellation tests exercise the actual command path.
	if restored := newComposer(queue[0].Text); restored.text != original {
		t.Fatal("restored composer lost original bytes")
	}
}

func BenchmarkPendingInputPreview(b *testing.B) {
	for _, size := range []struct {
		name string
		n    int
	}{{"short", 16}, {"bounded-prefix", pendingPreviewBytes / 5}, {"megabyte", 1 << 18}} {
		b.Run(size.name, func(b *testing.B) {
			source := strings.Repeat("word ", size.n)
			b.ReportAllocs()
			for b.Loop() {
				_ = pendingInputPreview("Queued · ", source, 80)
			}
		})
	}
}

func TestPendingInputBoundedPreviewCancellationAndAdmission(t *testing.T) {
	for _, steer := range []bool{false, true} {
		name, command, label := "queue", "/cancel-queue", "Queued · "
		if steer {
			name, command, label = "steer", "/cancel-steer", "Steer · "
		}
		t.Run(name, func(t *testing.T) {
			u, p := newCancelInputUI(t, []provider.ScriptResponse{{Text: "Initial settled."}, {Text: "Original admitted."}})
			original := "  original " + strings.Repeat("word \t", pendingPreviewBytes/6+1) + "\n e\u0301👩‍💻 final suffix  "
			pasteCancelInput(u, original)
			submitCancelInput(u, steer)
			u.wait(t, label+"original")
			u.typeText(command)
			u.key(tcell.KeyEnter)
			if frame := u.wait(t, "Cancelled pending input"); !strings.Contains(frame, "final suffix") {
				t.Fatal("cancellation restored only the bounded preview", frame)
			}
			submitCancelInput(u, steer)
			u.wait(t, label+"original")
			close(p.release)
			u.wait(t, "Original admitted.")
			if got := latestHumanInput(cancelTestRequest(t, p)); got.Content != original {
				t.Fatalf("admission changed authored bytes: got %d bytes, want %d", len(got.Content), len(original))
			}
		})
	}
}
