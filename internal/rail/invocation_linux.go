package rail

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

// prepareCommand materializes planned payloads as sealed anonymous files. The
// caller must call the returned cleanup on every path, immediately after Start
// on success. Bubblewrap consumes the inherited copies; no staging pathname is
// exposed in the sandbox and no input descriptor reaches the sandbox command.
func (v invocation) prepareCommand(ctx context.Context) (*exec.Cmd, func(), error) {
	cmd := exec.CommandContext(ctx, v.executable, v.args...)
	cmd.Env = v.environment
	cleanup := func() {
		for _, file := range cmd.ExtraFiles {
			file.Close()
		}
		cmd.ExtraFiles = nil
	}
	for _, data := range v.files {
		if err := ctx.Err(); err != nil {
			cleanup()
			return nil, nil, err
		}
		fd, err := unix.MemfdCreate("ttc-rail-config", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("rail create anonymous config: %w", err)
		}
		file := os.NewFile(uintptr(fd), "ttc-rail-config")
		cmd.ExtraFiles = append(cmd.ExtraFiles, file)
		if _, err := file.WriteString(data); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("rail write anonymous config: %w", err)
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("rail rewind anonymous config: %w", err)
		}
		if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("rail seal anonymous config: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return nil, nil, err
	}
	return cmd, cleanup, nil
}
