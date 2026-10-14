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
	command := watchCommand(identity.Command)
	options := TaskOptions{Kind: "watch", Label: command, Background: true, WakeOnExit: wake, recordDir: dir, Inspector: func(Snapshot) InspectionSpec {
		return InspectionSpec{Fields: []InspectionField{{Name: "Record dir", Value: dir}, {Name: "Command", Value: command, Language: "sh"}}, Streams: defaultInspectionStreams(false)}
	}}
	return m.StartTask(owner, options, func(ctx context.Context, out, stderr io.Writer) (TaskResult, error) {
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

// watchCommand preserves recorded argv boundaries in a shell-readable display.
func watchCommand(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = arg
		if arg == "" || strings.ContainsFunc(arg, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r))
		}) {
			quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
		}
	}
	return strings.Join(quoted, " ")
}
