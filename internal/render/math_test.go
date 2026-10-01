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
