package history

import (
	"path/filepath"
	"testing"
)

func TestDataRootExplicitAndXDG(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "xdg"))
	t.Setenv("TTC_DATA_DIR", "")
	if root, err := DataRoot(); err != nil || root != filepath.Join(home, "xdg", "ttc") {
		t.Fatalf("XDG default: %s %v", root, err)
	}
	t.Setenv("TTC_DATA_DIR", filepath.Join(home, "custom"))
	if root, err := DataRoot(); err != nil || root != filepath.Join(home, "custom") {
		t.Fatalf("explicit default: %s %v", root, err)
	}
	t.Setenv("TTC_DATA_DIR", "relative")
	if _, err := DataRoot(); err == nil {
		t.Fatal("relative explicit data root accepted")
	}
	t.Setenv("TTC_DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")
	if root, err := DataRoot(); err != nil || root != filepath.Join(home, ".local", "share", "ttc") {
		t.Fatalf("home default: %s %v", root, err)
	}
}
