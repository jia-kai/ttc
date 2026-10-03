//go:build linux

package rail

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// AllowSSHAuthSock reads only the global rail config's TTC environment policy.
// Missing configuration or an absent setting returns false. It uses the same
// strict, bounded parser as LoadPolicy; invalid configurations return an error.
// An empty configHome selects home/.config. Both roots must otherwise be absolute.
func AllowSSHAuthSock(home, configHome string) (bool, error) {
	path, err := globalConfigPath(home, configHome)
	if err != nil {
		return false, err
	}
	parsed, err := loadPolicyFile(path, home, true)
	if err != nil {
		return false, err
	}
	return parsed.ttcAllowSSHAuthSock, nil
}

func globalConfigPath(home, configHome string) (string, error) {
	if err := absoluteRoot("home", home); err != nil {
		return "", err
	}
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	if err := absoluteRoot("config home", configHome); err != nil {
		return "", err
	}
	return filepath.Join(configHome, "ttc", "rail.json"), nil
}

// LoadPolicy reads configHome/ttc/rail.json and workdir/ttc-rail.json, skipping
// missing files. An empty configHome uses home/.config; other roots must be
// absolute. Project allows replace global allows at the same destination, while
// denies always win. Service requests require global authorization even if denied.
//
// Files are limited to 1 MiB and reject unknown/duplicate keys and trailing JSON.
// Each file allows at most 256 combined allow/deny entries, including services
// and duplicates. The merged policy allows at most 256 mount/deny paths after
// exact-destination replacement and deduplication, before deny filtering.
// Paths expand only ~ and ~/; all other characters (including $) are literal.
// Relative paths use the named config's parent. Config files must be regular,
// nonsymlink, singly linked files so their entrypoints can be protected read-only.
func LoadPolicy(home, configHome, workdir string) (Policy, error) {
	globalPath, err := globalConfigPath(home, configHome)
	if err != nil {
		return Policy{}, err
	}
	if err := absoluteRoot("workdir", workdir); err != nil {
		return Policy{}, err
	}
	global, err := loadPolicyFile(globalPath, home, true)
	if err != nil {
		return Policy{}, err
	}
	project, err := loadPolicyFile(filepath.Join(workdir, "ttc-rail.json"), home, false)
	if err != nil {
		return Policy{}, err
	}
	return resolvePolicy(global, project)
}

// loadPolicyFile supplies the parser with the named path's parent for relative
// entries, while retaining the canonical file path for read-only protection.
func loadPolicyFile(path, home string, global bool) (filePolicy, error) {
	data, realPath, err := readPolicyFile(path)
	if err != nil {
		return filePolicy{}, fmt.Errorf("rail config %s: %w", path, err)
	}
	if data == nil {
		return filePolicy{}, nil
	}
	parsed, err := parsePolicy(data, filepath.Dir(path), home, global)
	if err != nil {
		return filePolicy{}, fmt.Errorf("rail config %s: %w", path, err)
	}
	parsed.configFile = realPath
	return parsed, nil
}

// Hardlinks can hide writable config or service aliases inside arbitrary
// directories. Reject them rather than recursively auditing those directories.
func checkSingleLink(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot inspect link count")
	}
	if stat.Nlink > 1 {
		return fmt.Errorf("multiple hardlinks are unsupported")
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
	if err := checkSingleLink(info); err != nil {
		return nil, "", err
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
