package skills

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"
)

const maxInstructionBytes = 1 << 20

// ReadInstruction follows symlinks and reads a regular instruction file of at
// most 1 MiB. Special files are rejected without waiting for a FIFO writer.
// Cancellation closes the file; errors include the selected document's path.
func ReadInstruction(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()
	info, err := f.Stat()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("inspect instruction %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("instruction %s must be a regular file", path)
	}
	if info.Size() > maxInstructionBytes {
		return nil, fmt.Errorf("instruction %s exceeds the 1 MiB limit; shorten the document", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxInstructionBytes+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("read instruction %s: %w", path, err)
	}
	if len(b) > maxInstructionBytes {
		return nil, fmt.Errorf("instruction %s exceeds the 1 MiB limit; shorten the document", path)
	}
	return b, nil
}
