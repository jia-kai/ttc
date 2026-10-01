package capture

import (
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestRingWrapEOFCursorsAndEviction(t *testing.T) {
	p := NewPool(32)
	b := p.NewBuffer(12)
	b.Write([]byte("old\none\n"))
	b.Write([]byte("two\nthree\n"))
	for cursor, want := range map[string]string{"": "e\ntwo\nthree\n", "eof:-1:lines": "three\n", "eof:-2:lines": "two\nthree\n", "eof:-6:bytes": "three\n", "0": "e\ntwo\nthree\n"} {
		page, err := b.Read(cursor, 32)
		if err != nil || page.Output != want || !page.Truncated || page.NextCursor != "18" {
			t.Fatalf("%s: %+v %v", cursor, page, err)
		}
	}
	page, _ := b.Read("eof:-1:lines", 32)
	if page.Start != 12 || page.Line != 4 {
		t.Fatal(page)
	}
	other := p.NewBuffer(16)
	other.Write([]byte(strings.Repeat("x", 16)))
	third := p.NewBuffer(16)
	third.Write([]byte(strings.Repeat("z", 16)))
	page, _ = b.Read("", 32)
	if page.Output != "" || !page.Truncated {
		t.Fatal("oldest ring was not evicted", page)
	}
	used, limit := p.Stats()
	if used > limit || used != 32 {
		t.Fatal(used, limit)
	}
	p.Clear()
	used, _ = p.Stats()
	if used != 0 {
		t.Fatal("runtime retained allocation after close")
	}
}

func TestTailAndPageBoundaries(t *testing.T) {
	b := NewPool(8192).NewBuffer(4096)
	b.Write([]byte("one\ntwo\nthree"))
	if b.Tail(2, 1024) != "two\nthree" || b.Tail(10, 4) != "hree" {
		t.Fatal(b.Tail(2, 1024), b.Tail(10, 4))
	}
	b.Write([]byte("\n界界"))
	if got := b.Tail(10, 4); got != "界" || !utf8.ValidString(got) {
		t.Fatal(got)
	}
	if _, err := b.Read("eof:-6:bytes", 1); err == nil {
		t.Fatal("small UTF-8 page silently failed to advance")
	}
	for _, cursor := range []string{"-1", "1000", "eof:1:lines", "eof:-1:words", "eof:-9223372036854775808:bytes"} {
		if _, err := b.Read(cursor, 16); err == nil {
			t.Fatal("accepted", cursor)
		}
	}
	bad := NewPool(32).NewBuffer(16)
	bad.Write([]byte{0x80, '\n', 'x', '\n'})
	page, _ := bad.Read("", 16)
	if page.Output != "\x80\nx\n" || len(bad.Tail(10, 4)) > 4 {
		t.Fatal(page)
	}
}

func TestConcurrentSharedBudget(t *testing.T) {
	p := NewPool(256)
	var wg sync.WaitGroup
	for range 12 {
		b := p.NewBuffer(64)
		wg.Go(func() {
			for range 100 {
				b.Write([]byte("bounded output\n"))
				b.Read("eof:-1:lines", 16)
				b.Tail(10, 16)
				used, limit := p.Stats()
				if used > limit {
					t.Error(used, limit)
				}
			}
		})
	}
	wg.Wait()
	if CallLimit != 64<<20 || SharedLimit != 1<<30 {
		t.Fatal("unexpected defaults")
	}
}

func TestTailSkipsBlankLinesWithoutChangingRawOffsets(t *testing.T) {
	b := NewPool(16384).NewBuffer(8192)
	chunks := []string{"one\n\n", "two  \r", "\n", strings.Repeat(" \t\r\n", 500)}
	raw := strings.Join(chunks, "")
	for _, chunk := range chunks {
		b.Write([]byte(chunk))
	}
	if got := b.Tail(10, 1024); got != "one\n\ntwo  \r\n" {
		t.Fatalf("tail %q", got)
	}
	page, err := b.Read("", 8192)
	if err != nil || page.Output != raw || page.End != int64(len(raw)) {
		t.Fatal(page, err)
	}
	page, err = b.Read("eof:-1:lines", 128)
	if err != nil || page.Output != " \t\r\n" {
		t.Fatal("EOF cursor must keep blank lines", page, err)
	}
	b.Write([]byte("three"))
	if got := b.Tail(1, 1024); got != "three" {
		t.Fatalf("new nonblank line %q", got)
	}
	blank := NewPool(64).NewBuffer(32)
	blank.Write([]byte("\n \t\r\n"))
	if got := blank.Tail(10, 1024); got != "" {
		t.Fatalf("blank output %q", got)
	}
	blank.Write([]byte("lost\n" + strings.Repeat("\n", 40)))
	if got := blank.Tail(10, 1024); got != "" {
		t.Fatalf("evicted meaningful line returned %q", got)
	}
}
