package capture

import (
	"fmt"
	"strings"
	"testing"
)

func TestCleanSplitWritesAndPages(t *testing.T) {
	source := "first\n\x1b[31mred\x1b[0m\n\x1b]8;;https://example.org\x1b\\link\x1b]8;;\x1b\\\n"
	for split := 0; split <= len(source); split++ {
		p := NewPool(4096)
		b := p.NewBuffer(2048)
		b.Write([]byte(source[:split]))
		b.Write([]byte(source[split:]))
		page, err := b.ReadView("", 2048, true)
		if err != nil || page.Output != "first\nred\nlink\n" || page.NextCursor != fmt.Sprint(len(source)) {
			t.Fatalf("split %d: %+v %v", split, page, err)
		}
		// Each one-byte source page restores lexical context without exposing escape
		// suffixes. Plain text needs no terminal state inherited from another page.
		var joined strings.Builder
		for i := range len(source) {
			page, err = b.ReadView(fmt.Sprint(i), 1, true)
			if err != nil {
				t.Fatal(err)
			}
			joined.WriteString(page.Output)
		}
		if joined.String() != "first\nred\nlink\n" {
			t.Fatal(joined.String())
		}
		raw, _ := b.Read("", 2048)
		if raw.Output != source {
			t.Fatal("raw capture changed")
		}
	}
}

func TestCleanProgressAndNormalLines(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"before\n10%\r20%\r100%\x1b[K\nafter\n", "before\n100%\nafter\n"},
		{"abcdef\rxy\x1b[K\n", "xy\n"},
		{"abcdef\rxy\n", "xycdef\n"},
		{"abc\b\bXY\n", "aXY\n"},
		{"abc\x1b[2Kx\n", "   x\n"},
		{"\n \n\tunicode: 界🙂\n\n", "\n \n\tunicode: 界🙂\n\n"},
		{"\x00\x00\x01\n", "\x00\x00\x01\n"},
	} {
		p := NewPool(4096)
		b := p.NewBuffer(2048)
		for _, v := range []byte(tc.raw) {
			b.Write([]byte{v})
		}
		got, err := b.ReadView("", 2048, true)
		if err != nil || got.Output != tc.want {
			t.Fatalf("%q -> %q want %q (%v)", tc.raw, got.Output, tc.want, err)
		}
	}
}

func TestCleanRetentionAndBoundedReplay(t *testing.T) {
	p := NewPool(256)
	b := p.NewBuffer(32)
	source := "\x1b]52;;" + strings.Repeat("secret", 100) + "\aok\n"
	for _, v := range []byte(source) {
		b.Write([]byte{v})
	}
	got, err := b.ReadView("", 32, true)
	if err != nil || got.Output != "ok\n" || !got.Truncated {
		t.Fatalf("%+v %v", got, err)
	}
	raw, _ := b.Read("", 32)
	if raw.Output != source[len(source)-32:] {
		t.Fatal(raw)
	}

	p = NewPool(1 << 20)
	b = p.NewBuffer(1 << 19)
	source = "\x1b]52;;" + strings.Repeat("x", MaxCleanReplayBytes+10)
	b.Write([]byte(source))
	if _, err = b.ReadView("eof:-1:bytes", 1, true); err == nil {
		t.Fatal("missing bounded replay error")
	}
	if got, err = b.ReadView("eof:-1:bytes", 1, false); err != nil || got.Output != "x" {
		t.Fatal(got, err)
	}
	b.Write([]byte("\aok"))
	got, err = b.ReadView("eof:-2:bytes", 2, true)
	if err != nil || got.Output != "ok" {
		t.Fatal(got, err)
	}
	b.Write([]byte(strings.Repeat("a", cleanCheckpointBytes)))
	got, err = b.ReadView("eof:-2:bytes", 2, true)
	if err != nil || got.Output != "aa" {
		t.Fatal(got, err)
	}
	if len(b.clean.checkpoints) > b.size/cleanCheckpointBytes+2 || len(b.clean.prefix) > MaxCleanReplayBytes {
		t.Fatal("unbounded index")
	}
}

func TestCleanHeadTailIndependentAndRawOverride(t *testing.T) {
	p := NewPool(1024)
	b, empty := p.NewBuffer(512), p.NewBuffer(512)
	raw := "\x1b[31mred\x1b[0m--\x1b[32mgreen\x1b[0m"
	b.Write([]byte(raw))
	clean, _ := SelectOutput(b, empty, OutputOptions{Mode: "head_tail", Bytes: 16})
	off := false
	original, _ := SelectOutput(b, empty, OutputOptions{Mode: "head_tail", Bytes: 16, StripANSI: &off})
	if clean.CleaningError != "" || strings.Contains(clean.Output, "\x1b") || clean.HeadBytes != original.HeadBytes || clean.TailBytes != original.TailBytes || clean.OmittedBytes != original.OmittedBytes {
		t.Fatal(clean, original)
	}
	if !strings.Contains(original.Output, "\x1b") {
		t.Fatal(original)
	}
}

func TestCleanSharedEvictionPreservesLexicalState(t *testing.T) {
	p := NewPool(128)
	a, b, c := p.NewBuffer(64), p.NewBuffer(64), p.NewBuffer(64)
	a.Write([]byte("\x1b]52;;secret"))
	b.Write([]byte(strings.Repeat("b", 64)))
	c.Write([]byte(strings.Repeat("c", 64))) // Evicts a without changing its parser state.
	a.Write([]byte("still-secret\aok\n"))
	page, err := a.ReadView("", 64, true)
	if err != nil || page.Output != "ok\n" || !page.Truncated {
		t.Fatal(page, err)
	}
}

func TestPhysicalLineIndexAfterRetention(t *testing.T) {
	p := NewPool(1 << 17)
	b := p.NewBuffer(1 << 16)
	source := strings.Repeat("one\n\x1b]title\ninside\a\x1b[31mtext\x1b[0m\n", 4000)
	for start := 0; start < len(source); start += 1777 {
		b.Write([]byte(source[start:min(start+1777, len(source))]))
	}
	base := len(source) - b.size
	for start := base; start < len(source); start += 1337 {
		page, err := b.Read(fmt.Sprint(start), 100)
		want := int64(strings.Count(source[:page.Start], "\n") + 1)
		if err != nil || page.Line != want {
			t.Fatal(start, page, want, err)
		}
	}
}

func BenchmarkCleanTailIndexed(b *testing.B) {
	p := NewPool(8 << 20)
	buffer := p.NewBuffer(4 << 20)
	buffer.Write([]byte(strings.Repeat("\x1b[32mline\x1b[0m\n", 1<<18)))
	b.ResetTimer()
	for range b.N {
		if _, err := buffer.ReadView("eof:-4096:bytes", 4096, true); err != nil {
			b.Fatal(err)
		}
	}
}

func TestCleanRepeatedErasePreservesCursorWithoutMaterializingBlanks(t *testing.T) {
	for _, n := range []int{512, 2048, 8192} {
		p := NewPool(1 << 18)
		buffer := p.NewBuffer(1 << 17)
		source := strings.Repeat("a\x1b[2K", n) + "z\n"
		buffer.Write([]byte(source))
		page, err := buffer.ReadView("", len(source), true)
		if err != nil || page.Output != strings.Repeat(" ", n)+"z\n" {
			t.Fatal(n, page, err)
		}
	}
}

func TestCleanEvictionReleasesSourceIndexes(t *testing.T) {
	p := NewPool(1 << 17)
	a, b, c := p.NewBuffer(1<<16), p.NewBuffer(1<<16), p.NewBuffer(1<<16)
	a.Write([]byte(strings.Repeat("line\n", 6000)))
	if len(a.clean.checkpoints) == 0 || len(a.clean.lines) == 0 {
		t.Fatal("missing fixture indexes")
	}
	b.Write([]byte(strings.Repeat("b", 1<<16)))
	c.Write([]byte(strings.Repeat("c", 1<<16)))
	if a.size != 0 || a.clean.checkpoints != nil || a.clean.lines != nil || a.clean.prefix != nil {
		t.Fatal("evicted source allocation remains reachable")
	}
	a.Write([]byte("\x1b[31mnew\x1b[0m\n"))
	page, err := a.ReadView("", 100, true)
	if err != nil || page.Output != "new\n" || !page.Truncated {
		t.Fatal(page, err)
	}
}

func TestReadViewLargeSourceLimitDoesNotOverflow(t *testing.T) {
	p := NewPool(128)
	buffer := p.NewBuffer(64)
	buffer.Write([]byte("abc"))
	for _, strip := range []bool{false, true} {
		page, err := buffer.ReadView("1", int(^uint(0)>>1), strip)
		if err != nil || page.Output != "bc" || page.NextCursor != "3" {
			t.Fatal(page, err)
		}
	}
}

func BenchmarkCleanRepeatedErase(b *testing.B) {
	for _, n := range []int{512, 2048, 8192} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			p := NewPool(1 << 18)
			buffer := p.NewBuffer(1 << 17)
			source := strings.Repeat("a\x1b[2K", n) + "z\n"
			buffer.Write([]byte(source))
			b.SetBytes(int64(len(source)))
			b.ResetTimer()
			for range b.N {
				if _, err := buffer.ReadView("", len(source), true); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
