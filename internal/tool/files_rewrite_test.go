package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func TestFullRewriteAcrossAllActors(t *testing.T) {
	r, w, main, req := toolFixture(t)
	child, sibling := main, main
	child.Actor, sibling.Actor = "main/child", "main/sibling"
	write := func(x Execution, text string) Record {
		args, _ := json.Marshal(map[string]string{"path": "shared", "content": text})
		return invoke(t, r, w, x, req, "write", string(args))
	}
	reject := func(record Record) {
		t.Helper()
		if !strings.Contains(string(record.Result), `"ok":false`) || !strings.Contains(string(record.Result), "Full write rejected") || !strings.Contains(string(record.Result), "partial read qualifies") {
			t.Fatalf("expected actionable stale-write error: %s", record.Result)
		}
	}
	ok(t, write(main, "one\ntwo\nthree\n"))
	// Self-only writers need no intervening read, while never-reading children do.
	ok(t, write(main, "one\ntwo\nthree\nfour\n"))
	reject(write(child, "blind overwrite"))
	ok(t, invoke(t, r, w, child, req, "read", `{"path":"shared","offset":2,"limit":1}`))
	ok(t, write(child, "child\n"))
	reject(write(main, "stale parent"))
	reject(write(sibling, "blind sibling"))
	// A successful full write itself supplies an observation even with another
	// actor in the modification history.
	ok(t, write(child, "child again\n"))
	ok(t, invoke(t, r, w, main, req, "read", `{"path":"shared","limit":1}`))
	ok(t, write(main, "parent again\n"))
	reject(write(child, "stale child"))
	data, err := os.ReadFile(filepath.Join(w.Root, "shared"))
	if err != nil || string(data) != "parent again\n" {
		t.Fatalf("rejected write changed file: %q, %v", data, err)
	}
}

func TestTargetedMutationsInvalidateOtherActorsWithoutChangingEditContract(t *testing.T) {
	for _, test := range []struct {
		name, tool, args, readPath string
	}{
		{"edit", "edit", `{"path":"shared","old_text":"old","new_text":"new"}`, "shared"},
		{"patch", "patch", `{"patch_text":"*** Begin Patch\n*** Update File: shared\n@@\n-old\n+new\n*** End Patch"}`, "shared"},
		{"delete", "patch", `{"patch_text":"*** Begin Patch\n*** Delete File: shared\n*** End Patch"}`, "shared"},
		{"move-source", "patch", `{"patch_text":"*** Begin Patch\n*** Update File: shared\n*** Move to: moved\n@@\n-old\n+new\n*** End Patch"}`, "shared"},
		{"move-target", "patch", `{"patch_text":"*** Begin Patch\n*** Update File: shared\n*** Move to: moved\n@@\n-old\n+new\n*** End Patch"}`, "moved"},
		{"add", "patch", `{"patch_text":"*** Begin Patch\n*** Add File: added\n+new\n*** End Patch"}`, "added"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, w, main, req := toolFixture(t)
			child := main
			child.Actor = "main/child"
			ok(t, invoke(t, r, w, main, req, "write", `{"path":"shared","content":"old\n"}`))
			// Targeted changes remain allowed without imposing a fresh-read rule.
			ok(t, invoke(t, r, w, child, req, test.tool, test.args))
			args, _ := json.Marshal(map[string]string{"path": test.readPath, "content": "overwrite"})
			record := invoke(t, r, w, main, req, "write", string(args))
			if !strings.Contains(string(record.Result), "Full write rejected") {
				t.Fatalf("targeted mutation did not attribute actor: %s", record.Result)
			}
		})
	}
}

func TestReadSnapshotHashesFullStreamForPartialPages(t *testing.T) {
	for _, content := range []string{"", "one\ntwo\nthree\n", "first\n" + strings.Repeat("large suffix\n", 100000), "first\n" + strings.Repeat("x", 50000)} {
		path := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		page, checksum, err := readPageSnapshot(context.Background(), path, 1, 1, nil)
		expected := sha256.Sum256([]byte(content))
		if err != nil || checksum != hex.EncodeToString(expected[:]) {
			t.Fatalf("page/hash mismatch: %v, %q, %v", page, checksum, err)
		}
	}
}

func TestExternalChangesAndFailedReadRequireFreshRead(t *testing.T) {
	r, w, x, req := toolFixture(t)
	path := filepath.Join(w.Root, "file")
	ok(t, invoke(t, r, w, x, req, "write", `{"path":"file","content":"initial\n"}`))
	if err := os.WriteFile(path, []byte("external\n"), 0600); err != nil {
		t.Fatal(err)
	}
	record := invoke(t, r, w, x, req, "write", `{"path":"file","content":"stale"}`)
	if !strings.Contains(string(record.Result), "Full write rejected") {
		t.Fatal(string(record.Result))
	}
	ok(t, invoke(t, r, w, x, req, "read", `{"path":"file","limit":1}`))
	ok(t, invoke(t, r, w, x, req, "write", `{"path":"file","content":"fresh"}`))
	if err := os.WriteFile(path, []byte("\x00bad"), 0600); err != nil {
		t.Fatal(err)
	}
	record = invoke(t, r, w, x, req, "read", `{"path":"file"}`)
	if !strings.Contains(string(record.Result), `"ok":false`) {
		t.Fatal("invalid read succeeded", string(record.Result))
	}
	record = invoke(t, r, w, x, req, "write", `{"path":"file","content":"overwrite"}`)
	if !strings.Contains(string(record.Result), "Full write rejected") {
		t.Fatal("failed read credited an observation", string(record.Result))
	}
}

func TestBinaryReadObservationPreservesCapabilityAndPaginationChecks(t *testing.T) {
	r, w, main, req := toolFixture(t)
	child := main
	child.Actor = "main/child"
	child.BinaryFiles = []llm.BinaryFileType{documentCapability(".pdf")}
	args, err := json.Marshal(map[string]string{"path": "source.pdf", "content": string(documentFixture(t, ".pdf"))})
	if err != nil {
		t.Fatal(err)
	}
	ok(t, invoke(t, r, w, main, req, "write", string(args)))
	record := invoke(t, r, w, child, req, "read", `{"path":"source.pdf","offset":2}`)
	if !strings.Contains(string(record.Result), `"ok":false`) || len(record.Files) != 0 {
		t.Fatal("binary pagination behavior changed", string(record.Result))
	}
	record = invoke(t, r, w, child, req, "write", `{"path":"source.pdf","content":"replacement"}`)
	if !strings.Contains(string(record.Result), "Full write rejected") {
		t.Fatal("failed binary read credited an observation", string(record.Result))
	}
	record = invoke(t, r, w, child, req, "read", `{"path":"source.pdf"}`)
	ok(t, record)
	if len(record.Files) != 1 {
		t.Fatal("native binary attachment lost")
	}
	ok(t, invoke(t, r, w, child, req, "write", `{"path":"source.pdf","content":"replacement"}`))
}
