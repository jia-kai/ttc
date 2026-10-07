// Package scratch manages the private, ephemeral per-user experiment directory.
package scratch

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"ttc/internal/prompts"
)

// Verify creates or checks /tmp/ttc (shared, sticky 1777) and its effective-UID
// child (owned, private 0700). Symlinks and unexpected modes are rejected; an
// existing shared root may belong to another user. It returns the private path.
func Verify() (string, error) { return verifyAt("/tmp/ttc", os.Geteuid()) }
func verifyAt(root string, uid int) (string, error) {
	path := filepath.Join(root, strconv.Itoa(uid))
	for _, p := range []string{root, path} {
		mode := os.FileMode(0700)
		requirement := prompts.ScratchOwnedRequirement
		if p == root {
			mode = os.ModeSticky | 0777
			requirement = prompts.ScratchSharedRequirement
		}
		if err := os.Mkdir(p, mode); err == nil {
			// Mkdir applies umask; exact permissions are part of the contract.
			if err = os.Chmod(p, mode); err != nil {
				return "", fmt.Errorf(prompts.ScratchSetPermissions, p, err)
			}
		} else if !os.IsExist(err) {
			return "", fmt.Errorf(prompts.ScratchCreate, p, err)
		}
		st, err := os.Lstat(p)
		if err != nil {
			return "", fmt.Errorf(prompts.ScratchInspect, p, err)
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		actual := st.Mode() & (os.ModePerm | os.ModeSticky | os.ModeSetuid | os.ModeSetgid)
		if !ok || !st.IsDir() || actual != mode || (p != root && int(sys.Uid) != uid) {
			return "", fmt.Errorf(prompts.ScratchUnsafeDirectory, p, requirement)
		}
	}
	return path, nil
}
