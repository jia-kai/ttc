package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWebSearchConfigOperatorSettings(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := webSearchConfigPath()
	if err != nil || !strings.HasSuffix(path, "/ttc/web-search.json") {
		t.Fatal(path, err)
	}
	config, err := loadWebSearchConfig(path)
	if err != nil || config.APIKey != "" || config.Endpoint != "" {
		t.Fatal(config, err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte(`{"endpoint":"https://example.test/mcp","api_key":"synthetic-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	config, err = loadWebSearchConfig(path)
	if err != nil || config.APIKey != "synthetic-key" || config.Endpoint != "https://example.test/mcp" {
		t.Fatal("configuration not loaded", err)
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = loadWebSearchConfig(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatal(err)
	}
}

func TestWebSearchConfigRejectsMalformedInputWithoutSecrets(t *testing.T) {
	for _, text := range []string{`{"typo":true}`, `{"api_key":"synthetic-secret\n"}`, `{"endpoint":"file:///bad"}`, `{"api_key":"synthetic-secret"} {}`, `null`, strings.Repeat(" ", (64<<10)+1)} {
		path := filepath.Join(t.TempDir(), "web-search.json")
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadWebSearchConfig(path); err == nil || strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatal("invalid config/secret accepted", err)
		}
	}
}

func TestWebSearchConfigRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web-search.json")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := loadWebSearchConfig(path); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("configuration read blocked on FIFO")
	}
}
