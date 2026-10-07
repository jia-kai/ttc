package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for path, text := range map[string]string{
		"prompt/system.md":             "Stable instructions.\r\n\n",
		"prompt/child.md":              "\nChild instructions.\n",
		"prompt/btw.md":                "Read-only answer.",
		"prompt/compaction.yaml":       "instructions: Summarize.\ninput: 'Focus: %s %s'\nlinks: '%s Archives: %s %s'\n",
		"prompt/naming.yaml":           "text: Name it.\noutput_tokens: 32\ntimeout_seconds: 15\n",
		"prompt/tools.yaml":            "read:\n  description: Read a file.\n  notes:\n    missing: Try another path.\n",
		"prompt/runtime.yaml":          "RuntimeWarning: 'Synthetic warning: %s'\n",
		"prompt/tool-messages.yaml":    "ToolMissing: Synthetic missing input.\n",
		"internal/tool/register.go":    "package tool\nfunc setup() { Register(r, \"read\", description, properties) }\n",
		"internal/session/register.go": "package session\n",
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
	if !bytes.Contains(first, []byte(`"Stable instructions.\r\n\n"`)) || !bytes.Contains(first, []byte(`"Try another path."`)) || !bytes.Contains(first, []byte(`const RuntimeWarning = "Synthetic warning: %s"`)) {
		t.Fatal("lost original text", string(first))
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
	put(t, root, "prompt/system.md", "Edited source.\n")
	if err = generate(root, target); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(updated, []byte(`"Edited source.\n"`)) {
		t.Fatal("source edit not embedded", err)
	}
}

func TestInvalidAssetsLeaveOutputUntouched(t *testing.T) {
	for _, tc := range []struct{ name, path, text, want string }{
		{"empty", "prompt/system.md", "\n", "nonempty"},
		{"encoding", "prompt/system.md", "\xff", "UTF-8"},
		{"nul", "prompt/system.md", "a\x00b", "NUL"},
		{"unknown asset", "prompt/extra.txt", "other", "unsupported"},
		{"missing tool", "prompt/tools.yaml", "{}", "missing prompt"},
		{"extra tool", "prompt/tools.yaml", "write:\n  description: Write.\nread:\n  description: Read.\n", "unregistered"},
		{"unknown field", "prompt/tools.yaml", "read:\n  description: Read.\n  typo: bad\n", "field typo"},
		{"empty description", "prompt/tools.yaml", "read:\n  description: ''\n", "empty/invalid"},
		{"empty note", "prompt/tools.yaml", "read:\n  description: Read.\n  notes:\n    missing: ''\n", "invalid note"},
		{"duplicate", "prompt/tools.yaml", "read:\n  description: Read.\nread:\n  description: Other.\n", "already defined"},
		{"multiple documents", "prompt/tools.yaml", "read:\n  description: Read.\n---\n{}\n", "exactly one"},
		{"empty yaml", "prompt/naming.yaml", "", "EOF"},
		{"unknown config", "prompt/naming.yaml", "text: Name.\noutput_tokens: 32\ntimeout_seconds: 15\ntypo: 1", "field typo"},
		{"obsolete attempt limit", "prompt/naming.yaml", "text: Name.\noutput_tokens: 32\ntimeout_seconds: 15\nmax_attempts: 1", "field max_attempts"},
		{"invalid bounds", "prompt/naming.yaml", "text: Name.\noutput_tokens: 0\ntimeout_seconds: 15", "output_tokens"},
		{"empty compaction", "prompt/compaction.yaml", "instructions: ''\ninput: '%s %s'\nlinks: '%s %s %s'", "nonempty"},
		{"unknown compaction field", "prompt/compaction.yaml", "instructions: Summary.\ninput: '%s %s'\nlinks: '%s %s %s'\ntypo: bad", "field typo"},
		{"invalid template", "prompt/compaction.yaml", "instructions: Summary.\ninput: Focus %s\nlinks: '%s %s %s'", "format"},
		{"empty messages", "prompt/runtime.yaml", "{}", "model-facing messages"},
		{"empty message", "prompt/runtime.yaml", "RuntimeWarning: ''", "nonempty"},
		{"message nul", "prompt/runtime.yaml", "RuntimeWarning: \"bad\\0text\"", "NUL"},
		{"message number", "prompt/runtime.yaml", "RuntimeWarning: 42", "nonempty UTF-8"},
		{"message object", "prompt/runtime.yaml", "RuntimeWarning: {text: warning}", "nonempty UTF-8"},
		{"invalid message name", "prompt/runtime.yaml", "runtime_warning: warning", "invalid or reserved"},
		{"reserved message name", "prompt/runtime.yaml", "Naming: warning", "invalid or reserved"},
		{"markdown collision", "prompt/runtime.yaml", "Child: warning", "duplicate prompt"},
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
	root := fixture(t)
	if err := os.Remove(filepath.Join(root, "prompt/child.md")); err != nil {
		t.Fatal(err)
	}
	if err := generate(root, "generated.go"); err == nil || !strings.Contains(err.Error(), "missing Markdown") {
		t.Fatal(err)
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
	for _, name := range []string{"runtime.yaml", "tool-messages.yaml"} {
		root := fixture(t)
		if err := os.Remove(filepath.Join(root, "prompt", name)); err != nil {
			t.Fatal(err)
		}
		if err := generate(root, "generated.go"); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatal("missing required message source was not rejected", name, err)
		}
	}
}
