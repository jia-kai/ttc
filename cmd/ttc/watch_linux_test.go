package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestWatchRunCLI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "record")
	var out, stderr bytes.Buffer
	code, err := runWatch([]string{"--record-dir", dir, "--", "/bin/sh", "-c", "cat; printf error >&2; exit 6"}, strings.NewReader("stdin\n"), &out, &stderr)
	if err != nil || code != 6 || out.String() != "stdin\n" || stderr.String() != "error" {
		t.Fatalf("run code=%d err=%v stdout=%q stderr=%q", code, err, out.String(), stderr.String())
	}
	for _, args := range [][]string{{}, {"--record-dir", filepath.Join(t.TempDir(), "run"), "true"}, {"--record-dir", filepath.Join(t.TempDir(), "run"), "--"}} {
		code, err := runWatch(args, nil, io.Discard, io.Discard)
		if err == nil || code == 0 {
			t.Fatalf("accepted %v: %d %v", args, code, err)
		}
	}
	code, err = runWatch([]string{"--help"}, nil, io.Discard, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("help %d %v", code, err)
	}
}

func TestWatchRunCLIRejectsWorkloadSeparators(t *testing.T) {
	for _, valueIsSeparator := range []bool{false, true} {
		name := "ordinary record directory"
		if valueIsSeparator {
			name = "record directory value is separator"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			dir := "records"
			if valueIsSeparator {
				dir = "--"
			}
			marker := filepath.Join(root, "workload-ran")
			args := []string{"--record-dir", dir, "/bin/sh", "-c", `touch "$1"`, "fixture", marker, "--", "--"}
			code, err := runWatch(args, nil, io.Discard, io.Discard)
			if code != 1 || err == nil || err.Error() != "watch-run requires -- before the command" {
				t.Fatalf("accepted workload separator: code=%d err=%v", code, err)
			}
			for _, path := range []string{marker, filepath.Join(root, dir)} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("invalid boundary created %s: %v", path, err)
				}
			}
		})
	}
}

func TestWatchRunCLIPreservesCommandArguments(t *testing.T) {
	for _, dirValue := range []string{"records", "--", "--record-dir"} {
		for _, inline := range []bool{false, true} {
			t.Run(dirValue+"/inline="+strconv.FormatBool(inline), func(t *testing.T) {
				t.Chdir(t.TempDir())
				args := []string{"--record-dir", dirValue}
				if inline {
					args = []string{"--record-dir=" + dirValue}
				}
				args = append(args, "--", "/bin/sh", "-c", `printf '<%s>\n' "$@"`, "fixture", "--", "hello", "--", "--help", "--record-dir=workload")
				var out bytes.Buffer
				code, err := runWatch(args, nil, &out, io.Discard)
				want := "<-->\n<hello>\n<-->\n<--help>\n<--record-dir=workload>\n"
				if err != nil || code != 0 || out.String() != want {
					t.Fatalf("command args changed: code=%d err=%v stdout=%q", code, err, out.String())
				}
				if _, err := os.Stat(filepath.Join(dirValue, "completion.json")); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestWatchRunCLIHelpAndErrorsDoNotRunWorkload(t *testing.T) {
	for _, flagArg := range []string{"--help", "--unknown-wrapper-flag"} {
		t.Run(flagArg, func(t *testing.T) {
			root := t.TempDir()
			dir, marker := filepath.Join(root, "records"), filepath.Join(root, "workload-ran")
			args := []string{"--record-dir", dir, flagArg, "--", "/bin/sh", "-c", `touch "$1"`, "fixture", marker}
			code, err := runWatch(args, nil, io.Discard, io.Discard)
			if flagArg == "--help" {
				if code != 0 || err != nil {
					t.Fatalf("help code=%d err=%v", code, err)
				}
			} else if code != 1 || err == nil {
				t.Fatalf("unknown flag code=%d err=%v", code, err)
			}
			for _, path := range []string{marker, dir} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("flag handling created %s: %v", path, err)
				}
			}
		})
	}
}

func TestWatchRunCLIRejectsExistingRecordDirectory(t *testing.T) {
	root := t.TempDir()
	dir, marker := filepath.Join(root, "records"), filepath.Join(root, "workload-ran")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	args := []string{"--record-dir", dir, "--", "/bin/sh", "-c", `touch "$1"`, "fixture", marker}
	code, err := runWatch(args, nil, io.Discard, io.Discard)
	if code != 1 || err == nil {
		t.Fatalf("existing record directory code=%d err=%v", code, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("ran workload: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("existing record directory changed: %v %v", entries, err)
	}
}
