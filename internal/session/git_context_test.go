package session

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"ttc/internal/llm"
)

func TestInitialRuntimeEnvironmentIncludesCwdRepositoryAndBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git optional")
	}
	for _, repo := range []bool{false, true} {
		name := "plain"
		if repo {
			name = "repo"
		}
		t.Run(name, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			if repo {
				cmd := exec.Command("git", "-C", r.Workspace.Root, "init", "-b", "research")
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatal(err, string(output))
				}
			}
			nested := filepath.Join(r.Workspace.Root, "nested")
			if err := os.Mkdir(nested, 0700); err != nil {
				t.Fatal(err)
			}
			r.Workspace.Root = nested
			r.Provider = &childProvider{stream: func(_ context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
				if req.System != systemTemplate {
					t.Fatal("missing system instructions")
				}
				found := false
				for _, message := range req.Messages {
					if message.Role != "developer" || !message.Runtime {
						continue
					}
					var env runtimeContext
					if err := json.Unmarshal([]byte(message.Content), &env); err != nil {
						t.Fatal(err)
					}
					if env.Type != "runtime_context" {
						continue
					}
					if env.Cwd != nested || env.IsRepo != repo || repo && env.Branch != "research" || !repo && env.Branch != "" {
						t.Fatal(env)
					}
					found = true
				}
				if !found {
					t.Fatal("missing initial runtime environment")
				}
				return emit(llm.StreamEvent{Kind: "text", Text: "Done"})
			}}
			if err := r.Run(&llm.Message{Role: "user", Content: "Inspect environment"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
