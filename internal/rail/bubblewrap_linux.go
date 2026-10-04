package rail

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
)

// bubblewrapInvocation lowers a plan without consulting the host or changing
// its namespace, filesystem ordering, or environment choices.
func bubblewrapInvocation(plan sandboxPlan, process sandboxProcess) (invocation, error) {
	if plan.hostname == "" || strings.ContainsRune(plan.hostname, 0) {
		return invocation{}, fmt.Errorf("invalid sandbox hostname")
	}
	if !bubblewrapPath(plan.workdir) {
		return invocation{}, fmt.Errorf("invalid sandbox workdir %q", plan.workdir)
	}
	if len(process.command) == 0 || process.command[0] == "" {
		return invocation{}, fmt.Errorf("sandbox command is empty")
	}
	for i, arg := range process.command {
		if strings.ContainsRune(arg, 0) {
			return invocation{}, fmt.Errorf("sandbox command argument %d contains NUL", i)
		}
	}
	for i, entry := range process.environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !bubblewrapEnvironmentName(name) || strings.ContainsRune(value, 0) {
			return invocation{}, fmt.Errorf("invalid inherited environment entry %d", i)
		}
	}

	var args []string
	var files []string
	seen := make(map[namespaceKind]bool)
	for _, namespace := range plan.namespaces {
		if seen[namespace] {
			return invocation{}, fmt.Errorf("duplicate sandbox namespace %d", namespace)
		}
		seen[namespace] = true
		switch namespace {
		case namespaceUser:
			args = append(args, "--unshare-user")
		case namespaceProcess:
			args = append(args, "--unshare-pid")
		case namespaceHostname:
			args = append(args, "--unshare-uts")
		default:
			return invocation{}, fmt.Errorf("unknown sandbox namespace %d", namespace)
		}
	}
	args = append(args, "--hostname", plan.hostname)
	if plan.dropCapabilities {
		args = append(args, "--cap-drop", "ALL")
	}
	if plan.newSession {
		args = append(args, "--new-session")
	}
	if process.dieWithParent {
		args = append(args, "--die-with-parent")
	}
	for i, operation := range plan.filesystem {
		if err := operation.validateBubblewrap(); err != nil {
			return invocation{}, fmt.Errorf("filesystem operation %d: %w", i, err)
		}
		switch operation.kind {
		case filesystemBind:
			flag := "--ro-bind"
			if operation.writable {
				flag = "--bind"
			}
			args = append(args, flag, operation.source, operation.dest)
		case filesystemSymlink:
			args = append(args, "--symlink", operation.source, operation.dest)
		case filesystemProc:
			args = append(args, "--proc", operation.dest)
		case filesystemDevices:
			args = append(args, "--dev", operation.dest)
		case filesystemTmpfs:
			args = append(args, "--tmpfs", operation.dest)
			if !operation.writable {
				args = append(args, "--remount-ro", operation.dest)
			}
		case filesystemDirectory:
			mode := uint32(operation.mode.Perm())
			if operation.mode&fs.ModeSticky != 0 {
				mode |= 01000
			}
			if operation.mode&fs.ModeSetgid != 0 {
				mode |= 02000
			}
			if operation.mode&fs.ModeSetuid != 0 {
				mode |= 04000
			}
			args = append(args, "--perms", fmt.Sprintf("%04o", mode), "--dir", operation.dest)
		case filesystemFile:
			args = append(args, "--perms", "0600", "--ro-bind-data", strconv.Itoa(3+len(files)), operation.dest)
			files = append(files, operation.data)
		}
	}
	for i, change := range plan.environment {
		if !bubblewrapEnvironmentName(change.name) || strings.ContainsRune(change.value, 0) || (change.unset && change.value != "") {
			return invocation{}, fmt.Errorf("invalid environment change %d", i)
		}
		if change.unset {
			args = append(args, "--unsetenv", change.name)
		} else {
			args = append(args, "--setenv", change.name, change.value)
		}
	}
	args = append(args, "--chdir", plan.workdir, "--")
	args = append(args, process.command...)
	// A non-nil empty environment prevents exec.Cmd from inheriting live host state.
	environment := make([]string, len(process.environment))
	copy(environment, process.environment)
	return invocation{executable: "bwrap", args: args, environment: environment, files: files}, nil
}

func (operation filesystemOperation) validateBubblewrap() error {
	if !bubblewrapPath(operation.dest) {
		return fmt.Errorf("invalid destination %q", operation.dest)
	}
	if operation.kind != filesystemFile && operation.data != "" {
		return fmt.Errorf("data is only valid for generated files")
	}
	if operation.kind != filesystemDirectory && operation.mode != 0 {
		return fmt.Errorf("mode is only valid for directories")
	}
	if operation.kind != filesystemBind && operation.kind != filesystemTmpfs && operation.writable {
		return fmt.Errorf("writable is only valid for binds and tmpfs")
	}
	switch operation.kind {
	case filesystemBind:
		if !bubblewrapPath(operation.source) {
			return fmt.Errorf("invalid bind source %q", operation.source)
		}
	case filesystemSymlink:
		if operation.source == "" || strings.ContainsRune(operation.source, 0) {
			return fmt.Errorf("invalid symlink target")
		}
	case filesystemProc, filesystemDevices, filesystemTmpfs, filesystemDirectory, filesystemFile:
		if operation.source != "" {
			return fmt.Errorf("source is only valid for binds and symlinks")
		}
		if operation.kind == filesystemDirectory && operation.mode & ^(fs.ModePerm|fs.ModeSticky|fs.ModeSetuid|fs.ModeSetgid) != 0 {
			return fmt.Errorf("invalid directory permission bits")
		}
	default:
		return fmt.Errorf("unknown filesystem operation %d", operation.kind)
	}
	return nil
}

func bubblewrapPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}

func bubblewrapEnvironmentName(name string) bool {
	return name != "" && !strings.ContainsAny(name, "=\x00")
}
