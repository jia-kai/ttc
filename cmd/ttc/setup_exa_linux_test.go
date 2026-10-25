package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"ttc/internal/tool"
)

func TestSetupExa(t *testing.T) {
	t.Setenv("TTC_EXA_URL", "")
	for _, tc := range []struct {
		name, input, response      string
		status                     int
		existing, saved, wantError bool
	}{
		{"create", "synthetic-new\n", `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`, 200, false, true, false},
		{"overwrite", "synthetic-new\nyes\n", `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`, 200, true, true, false},
		{"decline", "synthetic-new\nno\n", "", 200, true, false, false},
		{"eof confirmation", "synthetic-new\n", "", 200, true, false, false},
		{"empty", "\n", "", 200, false, false, true},
		{"invalid", "synthetic new\n", "", 200, false, false, true},
		{"no input", "", "", 200, false, false, true},
		{"auth failure", "synthetic-new\ny\n", "synthetic-new", 401, true, false, true},
		{"rate limited", "synthetic-new\ny\n", "", 429, true, false, true},
		{"bad response", "synthetic-new\ny\n", "synthetic-new", 200, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("x-api-key") != "synthetic-new" {
					t.Error("missing key header")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.response)
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "config.json")
			// A transport rewrite keeps missing-config tests offline too.
			client := &http.Client{Transport: setupRoundTripper(func(r *http.Request) (*http.Response, error) {
				r.URL.Scheme = "http"
				r.URL.Host = strings.TrimPrefix(server.URL, "http://")
				return http.DefaultTransport.RoundTrip(r)
			})}
			var original []byte
			if tc.existing {
				original = []byte(`{"api_key":"synthetic-old","endpoint":"` + server.URL + `"}`)
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			err := setupExa(context.Background(), path, strings.NewReader(tc.input), &output, client)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(output.String(), "synthetic-new") || (err != nil && strings.Contains(err.Error(), "synthetic-new")) {
				t.Fatal("key leaked")
			}
			data, readErr := os.ReadFile(path)
			if tc.saved {
				if calls != 1 {
					t.Fatalf("calls = %d", calls)
				}
				config, err := loadWebSearchConfig(path)
				if err != nil || config.APIKey != "synthetic-new" {
					t.Fatal("key not saved", err)
				}
				if tc.existing && config.Endpoint != server.URL {
					t.Fatal("endpoint changed")
				}
				info, _ := os.Stat(path)
				if info.Mode().Perm() != 0600 {
					t.Fatal("config not private")
				}
			} else if tc.existing {
				if readErr != nil || !bytes.Equal(data, original) {
					t.Fatal("original changed", readErr)
				}
			} else if !os.IsNotExist(readErr) {
				t.Fatal("unexpected config", readErr)
			}
			if !tc.saved && !tc.wantError && calls != 0 {
				t.Fatal("declined setup made request")
			}
		})
	}
}

type setupRoundTripper func(*http.Request) (*http.Response, error)

func (f setupRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSetupExaRejectsUnsafeTargets(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "directory", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "config")
			other := filepath.Join(root, "other")
			if err := os.WriteFile(other, []byte(`{}`), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(other, path)
			case "hardlink":
				err = os.Link(other, path)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "malformed":
				err = os.WriteFile(path, []byte(`{`), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = setupExa(context.Background(), path, strings.NewReader("synthetic-new\ny\n"), io.Discard, http.DefaultClient); err == nil {
				t.Fatal("unsafe target accepted")
			}
		})
	}
}

func TestSaveSetupConfigDoesNotClobberChangedTarget(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "created", true: "replaced"}[existing], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			if existing {
				if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			original, err := setupConfigInfo(path)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err = os.WriteFile(path, []byte(`{"endpoint":"https://changed.test"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err = saveSetupConfig(path, original, tool.WebSearchConfig{APIKey: "synthetic-new"}); err == nil {
				t.Fatal("changed target overwritten")
			}
			data, _ := os.ReadFile(path)
			if strings.Contains(string(data), "synthetic-new") {
				t.Fatal("changed target overwritten")
			}
		})
	}
}

func TestSetupExaCLIStandalone(t *testing.T) {
	oldArgs, oldInput, oldClient := os.Args, os.Stdin, http.DefaultClient
	t.Cleanup(func() {
		os.Args, os.Stdin, http.DefaultClient = oldArgs, oldInput, oldClient
	})
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("TTC_DATA_DIR", "invalid-relative-data-root")
	t.Setenv("PATH", "") // Setup must not need ripgrep, provider auth, or history.
	t.Setenv("TTC_EXA_URL", "https://override.test")
	path := filepath.Join(root, "ttc", "web-search.json")
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	if _, err = io.WriteString(write, "synthetic-cli-key\n"); err != nil {
		t.Fatal(err)
	}
	write.Close()
	os.Stdin = read
	os.Args = []string{"ttc", "--setup-exa"}
	calls := 0
	http.DefaultClient = &http.Client{Transport: setupRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://override.test" || r.Header.Get("x-api-key") != "synthetic-cli-key" {
			t.Error("wrong verification request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`))}, nil
	})}
	if err = run(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("verification calls=%d", calls)
	}
	if config, err := loadWebSearchConfig(path); err != nil || config.APIKey != "synthetic-cli-key" || config.Endpoint != "" {
		t.Fatal("setup config missing", err)
	}
	for _, conflict := range []string{"--login", "--offline-script=missing", "--import-codex-auth=missing"} {
		os.Args = []string{"ttc", "--setup-exa", conflict}
		if err = run(); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Fatalf("conflict %s: %v", conflict, err)
		}
	}
}

func TestSetupExaIdleInputCancellation(t *testing.T) {
	for _, confirmation := range []bool{false, true} {
		t.Run(map[bool]string{false: "key", true: "confirmation"}[confirmation], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			if confirmation {
				if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			defer write.Close()
			if confirmation {
				if _, err = io.WriteString(write, "synthetic-key\n"); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			start := time.Now()
			err = setupExa(ctx, path, read, io.Discard, http.DefaultClient)
			if err == nil || !strings.Contains(err.Error(), "deadline exceeded") || time.Since(start) > time.Second {
				t.Fatalf("input cancellation: %v", err)
			}
		})
	}
}

func TestSetupExaRejectsPlaintextRemoteEndpoint(t *testing.T) {
	t.Setenv("TTC_EXA_URL", "http://remote.example/mcp")
	path := filepath.Join(t.TempDir(), "config")
	client := &http.Client{Transport: setupRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("insecure credential transmitted")
		return nil, nil
	})}
	err := setupExa(context.Background(), path, strings.NewReader("synthetic-key\n"), io.Discard, client)
	if err == nil || !strings.Contains(err.Error(), "requires HTTPS") {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("config saved despite insecure endpoint")
	}
	// A safe override must not permit saving a key beside an unsafe endpoint
	// that would expose it on the next launch without the override.
	t.Setenv("TTC_EXA_URL", "https://override.example/mcp")
	original := []byte(`{"endpoint":"http://remote.example/mcp"}`)
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	err = setupExa(context.Background(), path, strings.NewReader("synthetic-key\nyes\n"), io.Discard, client)
	if err == nil || !strings.Contains(err.Error(), "requires HTTPS") {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatal("unsafe stored config changed", err)
	}
}
