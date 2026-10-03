package tui

import (
	"context"
	"scicode/internal/workspace"
	"sync"
	"time"
)

// workspaceMonitor owns periodic Git queries; drawing never starts a process.
type workspaceMonitor struct {
	updates chan workspace.GitInfo
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func newWorkspaceMonitor(ctx context.Context, cwd string) *workspaceMonitor {
	ctx, cancel := context.WithCancel(ctx)
	m := &workspaceMonitor{updates: make(chan workspace.GitInfo, 1), cancel: cancel}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			v := workspace.InspectGit(ctx, cwd)
			select {
			case m.updates <- v:
			case <-ctx.Done():
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return m
}
func (m *workspaceMonitor) close() { m.cancel(); m.wg.Wait() }
