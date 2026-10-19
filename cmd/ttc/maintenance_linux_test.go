package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ttc/internal/history"
	"ttc/internal/historytest"
	"ttc/internal/llm"
)

func startupHistoryFixture(t *testing.T, large bool) (*history.Store, history.Session, string) {
	t.Helper()
	root, work := filepath.Join(t.TempDir(), "data"), t.TempDir()
	s, err := history.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	id := history.NewID("session")
	selection := llm.Selection{Provider: "script", Model: llm.ScriptModel(), Variant: "none"}
	turn, _, err := s.StartSession(id, work, selection, llm.Message{Type: llm.TypeUserInput, Content: "old fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTurn(turn, "completed"); err != nil {
		t.Fatal(err)
	}
	db := historytest.DB(t, s)
	if _, err := db.Exec("UPDATE sessions SET last_activity_ms=0"); err != nil {
		t.Fatal(err)
	}
	v, err := s.Session(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if large {
		// SQLite uses its header's logical page count; sparse trailing bytes
		// exercise the production size gate without allocating 2 GiB of data.
		if err := os.Truncate(filepath.Join(root, "history.sqlite"), 2<<30); err != nil {
			t.Fatal(err)
		}
	}
	s, err = history.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, v, work
}

func TestStartupMaintenanceSmallHistoryAndLoadedProtection(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(map[bool]string{false: "small", true: "protected large"}[large], func(t *testing.T) {
			s, v, _ := startupHistoryFixture(t, large)
			p, out, _ := testStartupProgress(false, 80)
			notices, err := maintainHistory(context.Background(), s, v.ID, p)
			if err != nil || len(notices) != 1 || !strings.Contains(notices[0], "History database:") || !strings.Contains(notices[0], "~") {
				t.Fatal(notices, err)
			}
			if !large && out.Len() != 0 {
				t.Fatal("small startup produced cleanup progress", out.String())
			}
			if strings.Contains(strings.Join(notices, "\n"), "removed") {
				t.Fatal("loaded family was deleted", notices)
			}
			if saved, err := s.Session(v.ID); err != nil || saved.LastActivityMS <= 0 {
				t.Fatal("loaded family activity was not refreshed", saved, err)
			}
		})
	}
}

func TestStartupMaintenanceReportsCommittedDeletionOnAssetFailure(t *testing.T) {
	s, v, _ := startupHistoryFixture(t, true)
	asset := filepath.Join(s.Root, "lineages", v.LineageID)
	if err := os.MkdirAll(asset, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(asset, 0755); err != nil {
		t.Fatal(err)
	}
	p, out, _ := testStartupProgress(false, 80)
	notices, err := maintainHistory(context.Background(), s, "", p)
	if err != nil || len(notices) != 2 || !strings.Contains(notices[0], "removed conversation families: 1") || !strings.Contains(notices[1], "~0 entries") {
		t.Fatal("partial-commit warning/statistics lost", notices, err)
	}
	if !strings.Contains(out.String(), "History maintenance failed.") {
		t.Fatal("failed sweep implied success", out.String())
	}
	if _, err := s.Session(v.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("fixture SQL deletion did not commit", err)
	}
}

type cancelOnProgress struct {
	out    *bytes.Buffer
	match  string
	cancel context.CancelFunc
}

func (w cancelOnProgress) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.match) {
		w.cancel()
	}
	return w.out.Write(p)
}

func TestStartupMaintenanceCancellation(t *testing.T) {
	for _, phase := range []string{"before work", "discovering", "removing deleted conversation assets"} {
		t.Run(phase, func(t *testing.T) {
			s, v, _ := startupHistoryFixture(t, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p, out, _ := testStartupProgress(false, 80)
			p.out = cancelOnProgress{out, phase, cancel}
			if phase == "before work" {
				cancel()
			}
			notices, err := maintainHistory(ctx, s, "", p)
			if !errors.Is(err, context.Canceled) || len(notices) != 0 {
				t.Fatal("canceled startup continued", notices, err)
			}
			committed := phase == "removing deleted conversation assets"
			if strings.Contains(err.Error(), "removed conversation families: 1") != committed {
				t.Fatal("incorrect committed-deletion report", err)
			}
			_, savedErr := s.Session(v.ID)
			if (committed && !errors.Is(savedErr, sql.ErrNoRows)) || (!committed && savedErr != nil) {
				t.Fatal("unexpected deletion state", savedErr)
			}
			if phase == "before work" {
				if out.Len() != 0 {
					t.Fatal("canceled before startup but reported progress", out.String())
				}
			} else if !strings.Contains(out.String(), "History maintenance failed.") {
				t.Fatal("canceled operation left progress unfinished", out.String())
			}
		})
	}
}

func TestCLIMaintenanceBeforeFrontend(t *testing.T) {
	if root := os.Getenv("TTC_MAINTENANCE_HELPER_DATA"); root != "" {
		flag.CommandLine = flag.NewFlagSet("ttc", flag.ContinueOnError)
		os.Args = []string{"ttc", "--plain", "--data-dir", root, "--workdir", os.Getenv("TTC_MAINTENANCE_HELPER_WORK"), "--offline-script", os.Getenv("TTC_MAINTENANCE_HELPER_SCRIPT")}
		if err := run(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("CLI requires ripgrep")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s, v, work := startupHistoryFixture(t, true)
	root := s.Root
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "offline.json")
	if err := os.WriteFile(script, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestCLIMaintenanceBeforeFrontend$")
	cmd.Env = append(os.Environ(), "TTC_MAINTENANCE_HELPER_DATA="+root, "TTC_MAINTENANCE_HELPER_WORK="+work, "TTC_MAINTENANCE_HELPER_SCRIPT="+script)
	cmd.Stdin = strings.NewReader("/quit\n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("CLI startup: %v\n%s", err, output)
	}
	text := string(output)
	finished, frontend := strings.Index(text, "History maintenance complete."), strings.Index(text, "TTC · Linux terminal agent")
	if finished < 0 || frontend < finished {
		t.Fatal("frontend/input preceded synchronous maintenance", text)
	}
	s, err = history.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Session(v.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("immediate quit canceled startup cleanup", err)
	}
}

func TestHistoryStorageStatus(t *testing.T) {
	now := time.Unix(50*86400, 0)
	for _, tc := range []struct {
		name   string
		result history.MaintenanceResult
		want   string
	}{
		{"normal", history.MaintenanceResult{Stats: history.StorageStats{Bytes: 432327884, Entries: 83817, OldestMS: now.Add(-12*24*time.Hour - 9*time.Hour - 36*time.Minute).UnixMilli()}}, "History database: 412.3 MiB · ~83,817 entries · oldest 12.4 days"},
		{"empty ignores timestamp", history.MaintenanceResult{Stats: history.StorageStats{Bytes: 1024, OldestMS: 1}}, "History database: 1.0 KiB · ~0 entries · oldest n/a (empty history)"},
		{"unix epoch is not empty", history.MaintenanceResult{Stats: history.StorageStats{Bytes: 2 << 30, Entries: 1}}, "History database: 2.0 GiB · ~1 entry · oldest 50.0 days"},
		{"future timestamp", history.MaintenanceResult{Stats: history.StorageStats{Bytes: 42, Entries: 2, OldestMS: now.Add(time.Hour).UnixMilli()}, RemovedLineages: 2}, "History database: 42.0 B · ~2 entries · oldest 0.0 days · removed 2 inactive conversation families"},
		{"one removed family", history.MaintenanceResult{Stats: history.StorageStats{}, RemovedLineages: 1}, "History database: 0.0 B · ~0 entries · oldest n/a (empty history) · removed 1 inactive conversation family"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := historyStorageStatus(tc.result, now); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}
