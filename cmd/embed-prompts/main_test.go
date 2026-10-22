package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const fixtureSystem = "System: \"Stable instructions.\\r\\n\\n\"\nCoding: \"Coding workflow.\\n\"\nMain: \"Main workflow.\\n\"\nChild: \"Child instructions.\\n\"\nBtw: Read-only answer.\n"

const fixtureCompactionFocus = "CompactionFocus: 'Compaction focus: %s'\n"

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for path, text := range map[string]string{
		"prompt/system.yaml":              fixtureSystem,
		"prompt/child-messages.yaml":      "ChildMessageNonempty: message must be nonempty UTF-8\n",
		"prompt/compaction.md":            "Summarize.\n",
		"prompt/compaction-messages.yaml": fixtureCompactionFocus + "CompactionContext: '%s Continue the task.'\n",
		"prompt/naming.md":                "Name it.\r\n\n",
		"prompt/tools.yaml":               "read:\n  description: Read a file.\n  notes:\n    missing: Try another path.\n",
		"prompt/runtime.yaml":             "RuntimeWarning: 'Synthetic warning: %s'\n",
		"prompt/tool-messages.yaml":       "ToolMissing: Synthetic missing input.\n",
		"internal/tool/register.go":       "package tool\nfunc setup() { Register(r, \"read\", description, properties) }\n",
		"internal/session/register.go":    "package session\n",
	} {
		put(t, root, path, text)
	}
	return root
}

func put(t *testing.T, root, path, text string) {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationPreservesTextAndIsDeterministic(t *testing.T) {
	root := fixture(t)
	const target = "internal/prompts/assets_generated.go"
	if err := generate(root, target); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, target)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(first, []byte(`"Stable instructions.\r\n\n"`)) || !bytes.Contains(first, []byte(`"Try another path."`)) || !bytes.Contains(first, []byte(`const RuntimeWarning = "Synthetic warning: %s"`)) || !bytes.Contains(first, []byte(`const Child = "Child instructions.\n"`)) || !bytes.Contains(first, []byte(`const ChildMessageNonempty = "message must be nonempty UTF-8"`)) {
		t.Fatal("lost original text", string(first))
	}
	if !bytes.Contains(first, []byte(`const Naming = "Name it.\r\n\n"`)) {
		t.Fatal("lost generic naming asset text", string(first))
	}
	if bytes.Contains(first, []byte("NamingSettings")) || bytes.Contains(first, []byte("var naming")) {
		t.Fatal("generated naming runtime settings", string(first))
	}
	if err = generate(root, target); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || info.ModTime() != after.ModTime() {
		t.Fatal("unchanged generation rewrote output")
	}
	put(t, root, "prompt/system.yaml", strings.Replace(fixtureSystem, "Stable instructions.\\r\\n\\n", "Edited source.\\n", 1))
	if err = generate(root, target); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(updated, []byte(`"Edited source.\n"`)) {
		t.Fatal("source edit not embedded", err)
	}
}

func TestGenerationPreservesMarkdownAndYAMLBlockSemantics(t *testing.T) {
	root := fixture(t)
	const markdown = "Summarize the\nconversation.\n\n## Next steps\n\n- Return only the handoff.\n\n```text\n  Keep indentation.\n```\n"
	put(t, root, "prompt/system.yaml", strings.Replace(fixtureSystem,
		"Child: \"Child instructions.\\n\"\n",
		"Child: >\n  First wrapped\n  paragraph.\n\n  Second paragraph.\n", 1))
	put(t, root, "prompt/compaction.md", markdown)
	put(t, root, "prompt/compaction-messages.yaml", "CompactionFocus: >-\n  Compaction focus:\n  %s\nCompactionContext: \"Continue using the summary below.\\n\\n%s\"\n")
	if err := generate(root, "generated.go"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`const Child = "First wrapped paragraph.\nSecond paragraph.\n"`,
		"const Compaction = " + strconv.Quote(markdown),
		`const CompactionFocus = "Compaction focus: %s"`,
		`const CompactionContext = "Continue using the summary below.\n\n%s"`,
	} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("generated output missing %s", want)
		}
	}
}

func TestInvalidAssetsLeaveOutputUntouched(t *testing.T) {
	for _, tc := range []struct{ name, path, text, want string }{
		{"empty", "prompt/system.yaml", "System: ''", "nonempty"},
		{"encoding", "prompt/system.yaml", "System: \xff", "UTF-8"},
		{"nul", "prompt/system.yaml", "System: \"a\\0b\"", "NUL"},
		{"system number", "prompt/system.yaml", "System: 42", "nonempty UTF-8"},
		{"system duplicate", "prompt/system.yaml", "System: one\nSystem: two", "already defined"},
		{"system documents", "prompt/system.yaml", fixtureSystem + "---\nOther: other", "exactly one"},
		{"unknown role", "prompt/system.yaml", fixtureSystem + "Typo: instructions\n", "unknown role"},
		{"unknown asset", "prompt/extra.txt", "other", "unsupported"},
		{"missing tool", "prompt/tools.yaml", "{}", "missing prompt"},
		{"extra tool", "prompt/tools.yaml", "write:\n  description: Write.\nread:\n  description: Read.\n", "unregistered"},
		{"unknown field", "prompt/tools.yaml", "read:\n  description: Read.\n  typo: bad\n", "field typo"},
		{"empty description", "prompt/tools.yaml", "read:\n  description: ''\n", "empty/invalid"},
		{"empty note", "prompt/tools.yaml", "read:\n  description: Read.\n  notes:\n    missing: ''\n", "invalid note"},
		{"duplicate", "prompt/tools.yaml", "read:\n  description: Read.\nread:\n  description: Other.\n", "already defined"},
		{"multiple documents", "prompt/tools.yaml", "read:\n  description: Read.\n---\n{}\n", "exactly one"},
		{"obsolete naming yaml", "prompt/naming.yaml", "text: Name.\noutput_tokens: 32\ntimeout_seconds: 15\n", "unsupported prompt asset naming.yaml"},
		{"empty naming", "prompt/naming.md", "", "nonempty UTF-8"},
		{"naming encoding", "prompt/naming.md", "\xff", "UTF-8"},
		{"naming nul", "prompt/naming.md", "Name\x00it.", "NUL"},
		{"obsolete compaction yaml", "prompt/compaction.yaml", "instructions: Summary.\nfocus: '%s'\ncontext: '%s'", "unsupported prompt asset compaction.yaml"},
		{"empty compaction", "prompt/compaction.md", "", "nonempty UTF-8"},
		{"compaction encoding", "prompt/compaction.md", "\xff", "UTF-8"},
		{"compaction nul", "prompt/compaction.md", "Summary\x00.", "NUL"},
		{"invalid focus template", "prompt/compaction-messages.yaml", "CompactionFocus: Focus\nCompactionContext: '%s'", "format"},
		{"invalid summary template", "prompt/compaction.md", "Summary %s", "format"},
		{"empty context", "prompt/compaction-messages.yaml", fixtureCompactionFocus + "CompactionContext: ''", "nonempty UTF-8"},
		{"missing context slot", "prompt/compaction-messages.yaml", fixtureCompactionFocus + "CompactionContext: Continue.", "format"},
		{"extra context slot", "prompt/compaction-messages.yaml", fixtureCompactionFocus + "CompactionContext: '%s %s'", "format"},
		{"invalid context format", "prompt/compaction-messages.yaml", fixtureCompactionFocus + "CompactionContext: '%s %d'", "format"},
		{"compaction collision", "prompt/runtime.yaml", "Compaction: Duplicate.", "duplicate prompt"},
		{"compaction context collision", "prompt/runtime.yaml", "CompactionContext: '%s'", "duplicate prompt"},
		{"empty messages", "prompt/runtime.yaml", "{}", "model-facing messages"},
		{"empty message", "prompt/runtime.yaml", "RuntimeWarning: ''", "nonempty"},
		{"message nul", "prompt/runtime.yaml", "RuntimeWarning: \"bad\\0text\"", "NUL"},
		{"message number", "prompt/runtime.yaml", "RuntimeWarning: 42", "nonempty UTF-8"},
		{"message object", "prompt/runtime.yaml", "RuntimeWarning: {text: warning}", "nonempty UTF-8"},
		{"invalid message name", "prompt/runtime.yaml", "runtime_warning: warning", "invalid or reserved"},
		{"reserved message name", "prompt/runtime.yaml", "ToolDescription: warning", "invalid or reserved"},
		{"naming collision", "prompt/runtime.yaml", "Naming: warning", "duplicate prompt name Naming"},
		{"system collision", "prompt/runtime.yaml", "System: warning", "must be defined in system.yaml"},
		{"obsolete markdown", "prompt/system.md", "Old instructions.", "must be defined in system.yaml"},
		{"child collision", "prompt/runtime.yaml", "Child: warning", "must be defined in system.yaml"},
		{"message collision", "prompt/tool-messages.yaml", "RuntimeWarning: warning", "duplicate prompt"},
		{"duplicate message", "prompt/runtime.yaml", "RuntimeWarning: warning\nRuntimeWarning: other", "already defined"},
		{"invalid extra message", "prompt/helper-messages.yaml", "ExtraMessage: false", "nonempty UTF-8"},
		{"extra message collision", "prompt/helper-messages.yaml", "ToolMissing: duplicate", "duplicate prompt"},
		{"message documents", "prompt/runtime.yaml", "RuntimeWarning: warning\n---\nOther: other", "exactly one"},
		{"message encoding", "prompt/runtime.yaml", "RuntimeWarning: \xff", "UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := fixture(t)
			const target = "internal/prompts/assets_generated.go"
			if err := generate(root, target); err != nil {
				t.Fatal(err)
			}
			prior, _ := os.ReadFile(filepath.Join(root, target))
			put(t, root, tc.path, tc.text)
			err := generate(root, target)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			after, _ := os.ReadFile(filepath.Join(root, target))
			if !bytes.Equal(prior, after) {
				t.Fatal("failed validation replaced prior output")
			}
		})
	}
}

func TestMissingRequiredPrompt(t *testing.T) {
	for _, name := range []string{"System", "Coding", "Main", "Child", "Btw"} {
		t.Run(name, func(t *testing.T) {
			root := fixture(t)
			var remaining strings.Builder
			for _, line := range strings.SplitAfter(fixtureSystem, "\n") {
				if !strings.HasPrefix(line, name+":") {
					remaining.WriteString(line)
				}
			}
			put(t, root, "prompt/system.yaml", remaining.String())
			if err := generate(root, "generated.go"); err == nil || !strings.Contains(err.Error(), "missing required prompt "+name) {
				t.Fatal(err)
			}
		})
	}
}

func TestAdditionalMessageAssetsAndSkillsSubtree(t *testing.T) {
	root := fixture(t)
	put(t, root, "prompt/helper-messages.yaml", "HelperWarning: 'first\\nsecond %q'\n")
	put(t, root, "prompt/skills/example/SKILL.md", "External skill embedding source.\n")
	if err := generate(root, "generated.go"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "generated.go"))
	if err != nil || !bytes.Contains(data, []byte(`const HelperWarning = "first\\nsecond %q"`)) {
		t.Fatal("additional message asset was not preserved", string(data), err)
	}
}

func TestMissingRequiredMessageAsset(t *testing.T) {
	for _, name := range []string{"system.yaml", "runtime.yaml", "tool-messages.yaml"} {
		root := fixture(t)
		if err := os.Remove(filepath.Join(root, "prompt", name)); err != nil {
			t.Fatal(err)
		}
		if err := generate(root, "generated.go"); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatal("missing required message source was not rejected", name, err)
		}
	}
}

func TestMissingRequiredCompactionPrompt(t *testing.T) {
	for symbol, remaining := range map[string]string{
		"Compaction":        "",
		"CompactionContext": fixtureCompactionFocus,
		"CompactionFocus":   "CompactionContext: '%s'\n",
	} {
		t.Run(symbol, func(t *testing.T) {
			root := fixture(t)
			if symbol == "Compaction" {
				if err := os.Remove(filepath.Join(root, "prompt", "compaction.md")); err != nil {
					t.Fatal(err)
				}
			} else {
				put(t, root, "prompt/compaction-messages.yaml", remaining)
			}
			if err := generate(root, "generated.go"); err == nil || !strings.Contains(err.Error(), "missing required prompt "+symbol) {
				t.Fatalf("missing %s was not rejected: %v", symbol, err)
			}
		})
	}
}
