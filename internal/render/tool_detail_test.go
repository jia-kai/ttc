package render

import (
	"strings"
	"testing"
)

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
