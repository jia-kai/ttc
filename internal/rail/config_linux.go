//go:build linux

package rail

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const maxConfigBytes = 1 << 20

// Mount exposes Source at the absolute container path Dest. Writable defaults
// to false. Source existence and file type are checked by the mount planner.
type Mount struct {
	Source   string
	Dest     string
	Writable bool
}

// Policy is the merged global and project filesystem policy.
type Policy struct {
	Mounts      []Mount
	Denies      []string // Absolute container paths; masks also cover descendants.
	ConfigFiles []string // Canonical existing config paths, for read-only mounts.
	Docker      bool     // Authorized and requested, and not denied.
}

type filePolicy struct {
	mounts     []Mount
	denies     []string
	request    bool
	denyDocker bool
	authorize  bool
}

// LoadPolicy reads configHome/ttc/rail.json and workdir/ttc-rail.json, skipping
// missing files. An empty configHome uses home/.config; other roots must be
// absolute. Project allows replace global allows at the same destination, while
// denies always win. Docker requests require global authorization even if denied.
//
// Files are limited to 1 MiB and reject unknown/duplicate keys and trailing JSON.
// Paths expand only ~ and ~/; all other characters (including $) are literal.
// Relative paths use the named config's parent. Config files must be regular,
// nonsymlink files so their workspace entrypoints can be protected read-only.
func LoadPolicy(home, configHome, workdir string) (Policy, error) {
	var result Policy
	if err := absoluteRoot("home", home); err != nil {
		return result, err
	}
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	if err := absoluteRoot("config home", configHome); err != nil {
		return result, err
	}
	if err := absoluteRoot("workdir", workdir); err != nil {
		return result, err
	}
	paths := []string{filepath.Join(configHome, "ttc", "rail.json"), filepath.Join(workdir, "ttc-rail.json")}
	var authorized, requested, denied bool
	mountIndex := make(map[string]int)
	deniedPaths := make(map[string]bool)
	for i, path := range paths {
		data, realPath, err := readPolicyFile(path)
		if err != nil {
			return Policy{}, fmt.Errorf("rail config %s: %w", path, err)
		}
		if data == nil {
			continue
		}
		parsed, err := parsePolicy(data, filepath.Dir(path), home, i == 0)
		if err != nil {
			return Policy{}, fmt.Errorf("rail config %s: %w", path, err)
		}
		result.ConfigFiles = appendUnique(result.ConfigFiles, realPath)
		for _, mount := range parsed.mounts {
			if j, exists := mountIndex[mount.Dest]; exists {
				result.Mounts[j] = mount
			} else {
				mountIndex[mount.Dest] = len(result.Mounts)
				result.Mounts = append(result.Mounts, mount)
			}
		}
		for _, dest := range parsed.denies {
			if !deniedPaths[dest] {
				result.Denies = append(result.Denies, dest)
				deniedPaths[dest] = true
			}
		}
		authorized = authorized || parsed.authorize
		requested = requested || parsed.request
		denied = denied || parsed.denyDocker
	}
	if requested && !authorized {
		return Policy{}, fmt.Errorf("docker requires authorize_services in the global rail config")
	}
	result.Docker = requested && !denied
	mounts := result.Mounts[:0]
	for _, mount := range result.Mounts {
		blocked := false
		for dest := mount.Dest; ; dest = filepath.Dir(dest) {
			if deniedPaths[dest] {
				blocked = true
				break
			}
			if dest == "/" {
				break
			}
		}
		if !blocked {
			mounts = append(mounts, mount)
		}
	}
	result.Mounts = mounts
	return result, nil
}

func absoluteRoot(name, path string) error {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return fmt.Errorf("%s must be an absolute path without NUL", name)
	}
	return nil
}

func readPolicyFile(path string) ([]byte, string, error) {
	// Do not follow a final symlink: its directory entry would remain replaceable
	// inside a writable workspace even after protecting the canonical target.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		// An absent config is optional; an existing invalid entry is not.
		if _, statErr := os.Lstat(path); os.IsNotExist(statErr) {
			return nil, "", nil
		}
	}
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxConfigBytes {
		return nil, "", fmt.Errorf("exceeds %d-byte limit", maxConfigBytes)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, "", err
	}
	return data, realPath, nil
}

func parsePolicy(data []byte, base, home string, global bool) (filePolicy, error) {
	var result filePolicy
	obj, err := strictObject(data, "allow", "deny", "authorize_services")
	if err != nil {
		return result, err
	}
	if auth, ok := obj["authorize_services"]; ok {
		if !global {
			return result, fmt.Errorf("authorize_services is only permitted in the global config")
		}
		entries, err := strictArray(auth)
		if err != nil {
			return result, fmt.Errorf("authorize_services: %w", err)
		}
		for _, entry := range entries {
			service, err := strictString(entry)
			if err != nil || service != "docker" {
				return result, fmt.Errorf("authorize_services supports only the string docker")
			}
			result.authorize = true
		}
	}
	for _, key := range []string{"allow", "deny"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		entries, err := strictArray(raw)
		if err != nil {
			return result, fmt.Errorf("%s: %w", key, err)
		}
		for i, entry := range entries {
			mount, docker, err := parseEntry(entry, base, home, key == "allow")
			if err != nil {
				return result, fmt.Errorf("%s[%d]: %w", key, i, err)
			}
			if key == "allow" {
				if docker {
					result.request = true
				} else {
					result.mounts = append(result.mounts, mount)
				}
			} else if docker {
				result.denyDocker = true
			} else {
				result.denies = append(result.denies, mount.Dest)
			}
		}
	}
	return result, nil
}

func parseEntry(raw []byte, base, home string, allow bool) (Mount, bool, error) {
	var mount Mount
	if path, err := strictString(raw); err == nil {
		if path == "docker" {
			return mount, true, nil
		}
		resolved, err := policyPath(path, base, home)
		return Mount{Source: resolved, Dest: resolved}, false, err
	}
	keys := []string{"source", "dest"}
	if allow {
		keys = append(keys, "mode")
	}
	obj, err := strictObject(raw, keys...)
	if err != nil {
		return mount, false, err
	}
	for _, key := range []string{"source", "dest"} {
		if value, ok := obj[key]; ok {
			path, err := strictString(value)
			if err != nil {
				return mount, false, fmt.Errorf("%s: %w", key, err)
			}
			resolved, err := policyPath(path, base, home)
			if err != nil {
				return mount, false, fmt.Errorf("%s: %w", key, err)
			}
			if key == "source" {
				mount.Source = resolved
			} else {
				mount.Dest = resolved
			}
		}
	}
	if mount.Source == "" && (allow || mount.Dest == "") {
		return mount, false, fmt.Errorf("source is required (deny may instead specify dest)")
	}
	if mount.Dest == "" {
		mount.Dest = mount.Source
	}
	if rawMode, ok := obj["mode"]; ok {
		mode, err := strictString(rawMode)
		if err != nil || (mode != "ro" && mode != "rw") {
			return mount, false, fmt.Errorf("mode must be ro or rw")
		}
		mount.Writable = mode == "rw"
	}
	return mount, false, nil
}

func policyPath(path, base, home string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("path must be nonempty and contain no NUL")
	}
	if path == "~" {
		path = home
	} else if strings.HasPrefix(path, "~/") {
		path = filepath.Join(home, path[2:])
	} else if strings.HasPrefix(path, "~") {
		return "", fmt.Errorf("only ~ and ~/ home expansion is supported")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.Clean(path), nil
}

func strictObject(raw []byte, keys ...string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, fmt.Errorf("expected an object")
	}
	result := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("expected an object key")
		}
		known := false
		for _, allowed := range keys {
			known = known || key == allowed
		}
		if !known {
			return nil, fmt.Errorf("unknown key %q", key)
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		result[key] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	var trailing json.RawMessage
	if err := d.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing JSON")
	}
	return result, nil
}

func strictArray(raw []byte) ([]json.RawMessage, error) {
	if len(raw) == 0 || raw[0] != '[' {
		return nil, fmt.Errorf("expected an array")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func strictString(raw []byte) (string, error) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", fmt.Errorf("expected a string")
	}
	var value string
	err := json.Unmarshal(raw, &value)
	return value, err
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
