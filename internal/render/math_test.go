package render

import (
	"strings"
	"testing"
)

func TestMathDelimitersAndCodePreservation(t *testing.T) {
	for _, test := range []struct{ source, want string }{
		{`before $x^2$ after`, "before FORMULA(x^2,false) after"},
		{`$$\frac{1}{n}$$`, `FORMULA(\frac{1}{n},true)`},
		{`\(\sqrt{x}\)`, `FORMULA(\sqrt{x},false)`},
		{`\[\alpha\]`, `FORMULA(\alpha,true)`},
		{"`$x^2$`", "`$x^2$`"},
		{"```tex\n$\\alpha$\n```", "```tex\n$\\alpha$\n```"},
		{"~~~tex\n$\\alpha$\n~~~", "~~~tex\n$\\alpha$\n~~~"},
		{"    $\\alpha$\n", "    $\\alpha$\n"},
		{`cost \$5`, `cost \$5`},
		{"Unclosed $math", "Unclosed $math"},
	} {
		got := Math(test.source, func(tex string, block bool) string {
			b := "false"
			if block {
				b = "true"
			}
			return "FORMULA(" + tex + "," + b + ")"
		})
		if got != test.want {
			t.Fatalf("%q: %q != %q", test.source, got, test.want)
		}
	}
}
func TestPlainMathKeepsTeX(t *testing.T) {
	got, err := Terminal(`inline $\alpha$ and $$\frac{1}{2}$$`, 80)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `\alpha`) || !strings.Contains(got, `\frac`) {
		t.Fatal(got)
	}
}

func TestMathChunkCutKeepsCodeAndUnsupportedFormulas(t *testing.T) {
	for _, test := range []struct {
		source string
		cut    int
	}{
		{"prefix `$x^2$` suffix", 10},
		{"```tex\n$x^2$\n```\n", 10},
		{`prefix \$x^2 suffix`, 11},
		{"prefix $unclosed", 12},
		{"prefix $" + strings.Repeat("x", 4097) + "$ suffix", 2000},
	} {
		if got := MathChunkCut(test.source, test.cut); got != test.cut {
			t.Fatal("code or unsupported formula moved the cut", test.source[:min(40, len(test.source))], got, test.cut)
		}
	}
	for _, expression := range []string{"$x^2$", "$$\nx^2\n$$", `\(x^2\)`, `\[x^2\]`} {
		source := "prefix " + expression + " suffix"
		for cut := len("prefix ") + 1; cut < len("prefix ")+len(expression); cut++ {
			if got := MathChunkCut(source, cut); got != len("prefix ") {
				t.Fatal("formula delimiter or body was split", expression, got, cut)
			}
		}
	}
}
