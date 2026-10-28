package render

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark/ast"
)

func TestToolDetailCompactListsWithBlocks(t *testing.T) {
	tests := []struct {
		name, args, result string
		want               []string
	}{
		{"shell", `{"background":true,"command":"printf '\u0060\u0060\u0060'\necho done","label":"a*b","timeout_seconds":3}`, `{"exit_code":0}`, []string{"Background: true", "printf '```'", "echo done", "Label: a*b", "Strict: true", "Timeout seconds: 3"}},
		{"read", `{"limit":10,"path":"note.md"}`, `{"content":"first\n\nthird","offset":1,"truncated":false}`, []string{"Limit: 10", "Path: note.md", "first", "third", "Offset: 1", "Truncated: false"}},
		{"nested", `{"outer":{"a":1,"block":"one\n\ntwo","z":2},"rows":[{"a":3,"block":"x\ny","z":4},"tail"],"special*key":"[value]"}`, `{"ok":true}`, []string{"Outer:", "A: 1", "one", "two", "Z: 2", "Rows:", "A: 3", "x", "y", "Z: 4", "tail", "Special*key: [value]"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			detail := Tool(tc.name, []byte(tc.args), []byte(tc.result)).Detail
			doc := ParseMarkdown(detail)
			_ = ast.Walk(doc.root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
				if list, ok := node.(*ast.List); entering && ok && !list.IsTight {
					t.Errorf("loose list in detail:\n%s", detail)
				}
				return ast.WalkContinue, nil
			})
			output, err := doc.Render(100, LiteralAsset)
			if err != nil {
				t.Fatal(err)
			}
			output = strings.ReplaceAll(ansi.Strip(output), "\u00a0", " ")
			output = strings.ReplaceAll(output, ":  ", ": ")
			for _, want := range tc.want {
				if !strings.Contains(output, want) {
					t.Errorf("rendered detail missing %q:\n%s", want, output)
				}
			}
			if tc.name == "shell" && !strings.Contains(output, "• Label: a*b\n• Strict: true\n• Timeout seconds: 3") {
				t.Errorf("scalar siblings are not compact:\n%s", output)
			}
			if tc.name == "nested" && (!strings.Contains(output, "  • A: 1\n  • Block:") || !strings.Contains(output, "  • Z: 4")) {
				t.Errorf("nested indentation lost:\n%s", output)
			}
			if tc.name == "read" || tc.name == "nested" {
				first, last := "first", "third"
				if tc.name == "nested" {
					first, last = "one", "two"
				}
				rows := strings.Split(output, "\n")
				found := false
				for i := 0; i+2 < len(rows); i++ {
					if strings.TrimSpace(rows[i]) == first && strings.TrimSpace(rows[i+1]) == "" && strings.TrimSpace(rows[i+2]) == last {
						found = true
					}
				}
				if !found {
					t.Errorf("intentional blank line lost:\n%s", output)
				}
			}
		})
	}
}

func TestToolParametersReadablePreciseAndSafe(t *testing.T) {
	md := Tool("question", []byte(`{"questions":[{"question":"Which?","options":["one","two"],"recommendation":"one"}]}`), []byte(`{"ok":true,"large":9007199254740993}`))
	for _, want := range []string{"**Questions:**", "**Question:**", "**Options:**", "**Recommendation:**", "9007199254740993"} {
		if !strings.Contains(md.Detail, want) {
			t.Fatal("missing", want, md.Detail)
		}
	}
	if strings.Contains(md.Detail, `"questions":`) || strings.Contains(md.Detail, `"large":`) {
		t.Fatal("primary JSON presentation", md.Detail)
	}
	md = Tool("shell", []byte("{\"command\":\"printf '```'\\necho \\u001b[31m\"}"), nil)
	if !strings.Contains(md.Detail, "**Strict:** true") || !strings.Contains(md.Detail, "````sh") || strings.ContainsRune(md.Detail, '\x1b') {
		t.Fatal(md.Detail)
	}
	if DiffBriefing(md.Summary) {
		t.Fatal("shell command treated as a multiline diff")
	}
	if !DiffBriefing("**write** · path\n\n" + Fence("+new", "diff")) {
		t.Fatal("short diff unrecognized")
	}
}

func TestJobPreviewLeavesIdentityToInspection(t *testing.T) {
	result := []byte(`{"job_id":"watch-job","kind":"watch","label":"python train.py","record_dir":"/runs/train"}`)
	for _, tool := range []string{"watch", "job"} {
		md := InspectedTool(tool, []byte(`{"record_dir":"/runs/train"}`), result)
		for _, want := range []string{"**Job id:**", "watch-job", "**Record dir:**", "/runs/train"} {
			if !strings.Contains(md.Detail, want) {
				t.Fatalf("%s inspector missing %q: %s", tool, want, md.Detail)
			}
		}
		if strings.Contains(md.Detail, "**Label:**") || strings.Contains(md.Detail, "python train.py") {
			t.Fatal(md.Detail)
		}
	}
	md := InspectedTool("subagent", nil, []byte(`{"job_id":"child-job","kind":"subagent","label":"Review"}`))
	if strings.Contains(md.Detail, "**Label:**") || strings.Contains(md.Detail, "**Command:**") {
		t.Fatal("job identity belongs to the inspection descriptor", md.Detail)
	}
}

func TestStandaloneJobRenderingRetainsIdentity(t *testing.T) {
	result := []byte(`{"job_id":"watch-job","kind":"watch","label":"python train.py","record_dir":"/resolved/train"}`)
	for _, name := range []string{"watch", "job_read", "job_stop"} {
		detail := Tool(name, nil, result).Detail
		for _, want := range []string{"**Label:**", "python train.py", "**Record dir:**", "/resolved/train"} {
			if !strings.Contains(detail, want) {
				t.Fatalf("%s lost standalone metadata %q: %s", name, want, detail)
			}
		}
	}
}
