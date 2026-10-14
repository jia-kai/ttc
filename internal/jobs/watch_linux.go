package jobs

import (
	"context"
	"io"
	"strings"

	"ttc/internal/watchrun"
)

// StartWatch observes an external wrapper using ordinary job capture, ownership,
// read and notification machinery. Stop and Close cancel observation only; they
// never signal the wrapper or its workload. Record logs remain externally owned.
func (m *Manager) StartWatch(owner, dir string, wake bool) (string, error) {
	identity, err := watchrun.ReadIdentity(dir)
	if err != nil {
		return "", err
	}
	return m.StartTask(owner, "watch", strings.Join(identity.Command, " "), true, wake, func(ctx context.Context, out, stderr io.Writer) (TaskResult, error) {
		completion, err := watchrun.Observe(ctx, dir, identity, out, stderr)
		result := TaskResult{}
		if completion.ID != "" {
			code := completion.ExitCode
			result.ExitCode = &code
			result.Signal = completion.Signal
		}
		return result, err
	})
}
