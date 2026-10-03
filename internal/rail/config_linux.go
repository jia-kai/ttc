//go:build linux

package rail

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

const maxConfigBytes = 1 << 20
const maxPolicyEntries = 256
const maxPolicyConfigFiles = 2

// Mount exposes Source at the absolute container path Dest. Writable defaults
// to false. Source existence and file type are checked by the mount planner.
type Mount struct {
	Source   string
	Dest     string
	Writable bool
}

// Policy is the merged global and project filesystem policy. Mounts and Denies
// together may contain at most 256 entries; ConfigFiles may contain at most the
// two global/project entrypoints. Specification construction also enforces these
// limits for policies constructed directly by callers.
type Policy struct {
	Mounts      []Mount
	Denies      []string // Absolute container paths; masks also cover descendants.
	ConfigFiles []string // Canonical existing config paths, for read-only mounts.
	Docker      bool     // Authorized and requested, and not denied.
	SSHAgent    bool     // Authorized and requested, and not denied.
}

// filePolicy holds parsed, lexically resolved paths and service choices for one
// config. File IO supplies configFile, the canonical read-only entrypoint.
type filePolicy struct {
	configFile          string
	mounts              []Mount
	denies              []string
	requests            map[string]bool
	deniedServices      map[string]bool
	authorizedServices  map[string]bool
	ttcAllowSSHAuthSock bool
}

// resolvePolicy merges parsed global/project layers without consulting the host.
// Exact destination replacements retain their original position. Limits apply
// after replacement/deduplication but before denies remove mounts. Returned slices
// are owned by the result; resolving does not mutate either layer.
func resolvePolicy(global, project filePolicy) (Policy, error) {
	var result Policy
	requested, denied := map[string]bool{}, map[string]bool{}
	mountIndex := make(map[string]int)
	deniedPaths := make(map[string]bool)
	for _, parsed := range []filePolicy{global, project} {
		if parsed.configFile != "" {
			result.ConfigFiles = appendUnique(result.ConfigFiles, parsed.configFile)
		}
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
		for _, service := range []string{"docker", "ssh-agent"} {
			requested[service] = requested[service] || parsed.requests[service]
			denied[service] = denied[service] || parsed.deniedServices[service]
		}
	}
	if err := checkPolicySize(result); err != nil {
		return Policy{}, err
	}
	for _, service := range []string{"docker", "ssh-agent"} {
		if requested[service] && !global.authorizedServices[service] {
			return Policy{}, fmt.Errorf("%s requires authorize_services in the global rail config", service)
		}
	}
	result.Docker = requested["docker"] && !denied["docker"]
	result.SSHAgent = requested["ssh-agent"] && !denied["ssh-agent"]
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

func checkPolicySize(policy Policy) error {
	if len(policy.Mounts)+len(policy.Denies) > maxPolicyEntries {
		return fmt.Errorf("rail policy exceeds %d combined mount/deny entries", maxPolicyEntries)
	}
	if len(policy.ConfigFiles) > maxPolicyConfigFiles {
		return fmt.Errorf("rail policy exceeds %d config files", maxPolicyConfigFiles)
	}
	return nil
}

func parsePolicy(data []byte, base, home string, global bool) (filePolicy, error) {
	result := filePolicy{requests: map[string]bool{}, deniedServices: map[string]bool{}, authorizedServices: map[string]bool{}}
	obj, err := strictObject(data, "allow", "deny", "authorize_services", "ttc_allow_ssh_auth_sock")
	if err != nil {
		return result, err
	}
	if raw, ok := obj["ttc_allow_ssh_auth_sock"]; ok {
		if !global {
			return result, fmt.Errorf("ttc_allow_ssh_auth_sock is only permitted in the global config")
		}
		if !bytes.Equal(raw, []byte("true")) && !bytes.Equal(raw, []byte("false")) {
			return result, fmt.Errorf("ttc_allow_ssh_auth_sock must be a boolean")
		}
		result.ttcAllowSSHAuthSock = bytes.Equal(raw, []byte("true"))
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
			if err != nil || !knownService(service) {
				return result, fmt.Errorf("authorize_services supports only the strings docker and ssh-agent")
			}
			result.authorizedServices[service] = true
		}
	}
	entryCount := 0
	for _, key := range []string{"allow", "deny"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		entries, err := strictArray(raw)
		if err != nil {
			return result, fmt.Errorf("%s: %w", key, err)
		}
		entryCount += len(entries)
		if entryCount > maxPolicyEntries {
			return result, fmt.Errorf("exceeds %d combined allow/deny entries", maxPolicyEntries)
		}
		for i, entry := range entries {
			mount, service, err := parseEntry(entry, base, home, key == "allow")
			if err != nil {
				return result, fmt.Errorf("%s[%d]: %w", key, i, err)
			}
			if key == "allow" {
				if service != "" {
					result.requests[service] = true
				} else {
					result.mounts = append(result.mounts, mount)
				}
			} else if service != "" {
				result.deniedServices[service] = true
			} else {
				result.denies = append(result.denies, mount.Dest)
			}
		}
	}
	return result, nil
}

func knownService(name string) bool { return name == "docker" || name == "ssh-agent" }

func parseEntry(raw []byte, base, home string, allow bool) (Mount, string, error) {
	var mount Mount
	if path, err := strictString(raw); err == nil {
		if knownService(path) {
			return mount, path, nil
		}
		resolved, err := policyPath(path, base, home)
		return Mount{Source: resolved, Dest: resolved}, "", err
	}
	keys := []string{"source", "dest"}
	if allow {
		keys = append(keys, "mode")
	}
	obj, err := strictObject(raw, keys...)
	if err != nil {
		return mount, "", err
	}
	for _, key := range []string{"source", "dest"} {
		if value, ok := obj[key]; ok {
			path, err := strictString(value)
			if err != nil {
				return mount, "", fmt.Errorf("%s: %w", key, err)
			}
			resolved, err := policyPath(path, base, home)
			if err != nil {
				return mount, "", fmt.Errorf("%s: %w", key, err)
			}
			if key == "source" {
				mount.Source = resolved
			} else {
				mount.Dest = resolved
			}
		}
	}
	if mount.Source == "" && (allow || mount.Dest == "") {
		return mount, "", fmt.Errorf("source is required (deny may instead specify dest)")
	}
	if mount.Dest == "" {
		mount.Dest = mount.Source
	}
	if rawMode, ok := obj["mode"]; ok {
		mode, err := strictString(rawMode)
		if err != nil || (mode != "ro" && mode != "rw") {
			return mount, "", fmt.Errorf("mode must be ro or rw")
		}
		mount.Writable = mode == "rw"
	}
	return mount, "", nil
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
