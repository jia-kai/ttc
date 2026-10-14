package tool

import (
	"context"
	"ttc/internal/jobs"
	"ttc/internal/prompts"
	"ttc/internal/workspace"
)

type watchArgs struct {
	RecordDir string `json:"record_dir"`
	Wake      *bool  `json:"wake_on_exit,omitempty"`
}

// AddWatch registers observation of externally owned watch-run records. The
// resulting job supports job_read/list/stop; stopping it never kills workload.
func AddWatch(r *Registry, m *jobs.Manager, w *workspace.Manager) {
	Register(r, "watch", prompts.ToolDescription("watch"), map[string]any{"record_dir": Property("string"), "wake_on_exit": Property("boolean")}, []string{"record_dir"}, func(a watchArgs) error { return Required("record_dir", a.RecordDir) }, func(ctx context.Context, x Execution, a watchArgs) (any, error) {
		id, err := m.StartWatch(x.Actor, w.Path(a.RecordDir), a.Wake == nil || *a.Wake)
		if err != nil {
			return nil, jobToolError(err)
		}
		v, err := m.View(x.Actor, id)
		return v, jobToolError(err)
	})
}
