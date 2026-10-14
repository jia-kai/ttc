package capture

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestOutputSelectionModesAndCombinedBudget(t *testing.T) {
	p := NewPool(128)
	a, b := p.NewBuffer(64), p.NewBuffer(64)
	a.Write([]byte("0123456789"))
	b.Write([]byte("stderr"))
	for _, tc := range []struct {
		mode, out, err string
		head, tail     int
	}{
		{"head", "0123", "stde", 4, 0},
		{"tail", "6789", "derr", 0, 4},
		{"head_tail", "0189", "strr", 2, 2},
	} {
		out, err := SelectOutput(a, b, OutputOptions{Mode: tc.mode, Bytes: 8})
		if out.Output != tc.out || err.Output != tc.err || out.HeadBytes != tc.head || out.TailBytes != tc.tail || out.OmittedBytes != 6 || err.OmittedBytes != 2 {
			t.Fatal(tc, out, err)
		}
	}
	out, err := SelectOutput(a, b, OutputOptions{Bytes: 16})
	if out.Output != "0123456789" || err.Output != "stderr" || out.OmittedBytes != 0 || err.OmittedBytes != 0 {
		t.Fatal(out, err)
	}
	empty := p.NewBuffer(64)
	out, err = SelectOutput(a, empty, OutputOptions{Bytes: 8})
	if out.Output != "01236789" || err.Output != "" {
		t.Fatal(out, err)
	}
}

func TestOutputSelectionUTF8LostPrefixAndTinyBudgets(t *testing.T) {
	p := NewPool(64)
	a, b := p.NewBuffer(16), p.NewBuffer(16)
	a.Write([]byte("界界界"))
	for _, mode := range []string{"head", "tail", "head_tail"} {
		for budget := 1; budget <= 10; budget++ {
			out, _ := SelectOutput(a, b, OutputOptions{Mode: mode, Bytes: budget})
			if !utf8.ValidString(out.Output) || len(out.Output) > budget || out.OmittedBytes != 9-int64(len(out.Output)) {
				t.Fatal(mode, budget, out)
			}
		}
	}
	a.Write([]byte(strings.Repeat("x", 32)))
	out, _ := SelectOutput(a, b, OutputOptions{Bytes: 8})
	if out.TotalBytes != 41 || out.LostPrefixBytes != 25 || out.OmittedBytes != 33 || len(out.Output) != 8 {
		t.Fatal(out)
	}
	// Invalid source bytes are safely represented without growing the payload.
	b.Write([]byte{0xff, 0xfe, 'a'})
	_, err := SelectOutput(a, b, OutputOptions{Bytes: 16})
	if err.Output != "?a" || err.OmittedBytes != 0 {
		t.Fatal(err)
	}
}

func TestOutputOptionsValidation(t *testing.T) {
	for _, options := range []OutputOptions{{Mode: "all"}, {Bytes: -1}, {Bytes: MaxOutputBytes + 1}} {
		if _, err := options.Normalize(); err == nil {
			t.Fatal(options)
		}
	}
	options, err := (OutputOptions{}).Normalize()
	if err != nil || options.Mode != "head_tail" || options.Bytes != DefaultOutputBytes {
		t.Fatal(options, err)
	}
}
