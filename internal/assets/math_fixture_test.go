package assets

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isolatedMathCache uses an already installed optional dependency without
// network access. Mutating tests receive a private copy of that package.
func isolatedMathCache(t testing.TB) {
	t.Helper()
	requireMathPrograms(t)
	source, err := mathRoot()
	if err != nil || !readyMath(context.Background(), source) {
		t.Skip("optional MathJax cache unavailable; run ttc --install-math for real renderer tests")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root, err := mathRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.CopyFS(root, os.DirFS(source)); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
}

func fakeMathInstall(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TTC_MATH_TEST_EXE", exe)
	t.Setenv("TTC_MATH_HELPER", "1")
	t.Setenv("TTC_MATH_TEST_NPM_CALLS", filepath.Join(t.TempDir(), "calls"))
	var data bytes.Buffer
	if err = png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "fixture.png")
	if err = os.WriteFile(file, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TTC_MATH_TEST_PNG", file)
	bin := t.TempDir()
	for name, body := range map[string]string{
		"node":         `exec "$TTC_MATH_TEST_EXE" -test.run=^TestMathNodeHelperProcess$ -- "$@"`,
		"npm":          `exec "$TTC_MATH_TEST_EXE" -test.run=^TestMathNPMHelperProcess$ -- "$@"`,
		"rsvg-convert": `exec /bin/cat "$TTC_MATH_TEST_PNG"`,
	} {
		if err = os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestMathNPMHelperProcess(t *testing.T) {
	if os.Getenv("TTC_MATH_HELPER") != "1" {
		return
	}
	calls, err := os.OpenFile(os.Getenv("TTC_MATH_TEST_NPM_CALLS"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = calls.WriteString("npm\n"); err != nil {
		t.Fatal(err)
	}
	calls.Close()
	for _, flag := range []string{"ci", "--ignore-scripts", "--no-audit", "--no-fund", "--package-lock=true", "--cache", "--prefix"} {
		found := false
		for _, arg := range os.Args {
			found = found || arg == flag
		}
		if !found {
			fmt.Fprintln(os.Stderr, "missing install flag", flag)
			os.Exit(2)
		}
	}
	for name, expected := range map[string]string{"package.json": mathPackage, "package-lock.json": mathLock} {
		data, err := os.ReadFile(name)
		if err != nil || string(data) != expected {
			fmt.Fprintln(os.Stderr, "installer changed pinned manifest", name)
			os.Exit(2)
		}
	}
	for _, name := range []string{"src", "mathjax-newcm-font"} {
		path := filepath.Join("node_modules", "@mathjax", name)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		data := fmt.Sprintf(`{"name":"@mathjax/%s","version":"%s"}`, name, mathjaxVersion)
		if err := os.WriteFile(filepath.Join(path, "package.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMathNodeHelperProcess(t *testing.T) {
	if os.Getenv("TTC_MATH_HELPER") != "1" {
		return
	}
	if os.Args[len(os.Args)-1] != "worker" {
		fmt.Print("<svg/>")
		os.Exit(0)
	}
	fmt.Println(`{"ready":true}`)
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct{ TeX string }
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			os.Exit(2)
		}
		switch request.TeX {
		case "stall":
			time.Sleep(30 * time.Second)
		case "malformed":
			fmt.Println("broken JSON")
		case "unsupported":
			fmt.Println(`{"error":"unsupported fixture"}`)
		default:
			fmt.Println(`{"svg":"<svg/>"}`)
		}
	}
	os.Exit(0)
}

func TestMathLockPinsEveryDownloadedPackage(t *testing.T) {
	var lock struct {
		Packages map[string]struct {
			Version, Resolved, Integrity string
			Dependencies                 map[string]string
		}
	}
	if err := json.Unmarshal([]byte(mathLock), &lock); err != nil {
		t.Fatal(err)
	}
	if lock.Packages[""].Dependencies["@mathjax/src"] != mathjaxVersion {
		t.Fatal("root MathJax version is not exact")
	}
	for path, pkg := range lock.Packages {
		if path == "" {
			continue
		}
		digest, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(pkg.Integrity, "sha512-"))
		if pkg.Version == "" || !strings.HasPrefix(pkg.Resolved, "https://registry.npmjs.org/") || !strings.HasPrefix(pkg.Integrity, "sha512-") || err != nil || len(digest) != sha512.Size {
			t.Errorf("package not hash pinned: %s", path)
		}
	}
}
