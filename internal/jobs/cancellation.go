package jobs

import "context"

// cleanupCancel records scope teardown on the live member instead of scanning
// the manager's retained job history for every ownership cleanup.
func (m *Manager) cleanupCancel(j *job) func() {
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		cancelJob(j, "runtime_cleanup")
	}
}

// cancelJob must be called with Manager.mu held. Preserve the first cause,
// including an execution deadline that fired before an explicit stop.
func cancelJob(j *job, reason string) {
	if j.view.Status == "running" && j.cancelReason == "" && j.ctx.Err() == nil {
		j.cancelReason = reason
	}
	j.cancel()
}

func cancellationReason(ctx context.Context, j *job) string {
	if ctx.Err() == context.DeadlineExceeded {
		return "execution_deadline"
	}
	if j.cancelReason != "" {
		return j.cancelReason
	}
	return "runtime_cleanup"
}
