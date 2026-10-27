package jobs

import (
	"context"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"ttc/internal/capture"
)

func TestStopLaunchIdentityWithoutOutput(t *testing.T) {
	for _, tc := range []struct {
		label, command, want string
		lsp                  bool
	}{
		{label: "Check tests", command: "printf secret", want: "Check tests"},
		{command: " printf\n secret ", want: "printf secret"},
		{label: "Shell command", command: "printf secret", want: "Shell command"},
		{command: "sleep 60", want: "sleep 60", lsp: true},
		{label: "Language server", command: "sleep 60", want: "Language server", lsp: true},
		{command: " ", want: ""},
		{label: strings.Repeat("界", 100), command: "true", want: strings.Repeat("界", 79) + "…"},
	} {
		t.Run(tc.label+tc.command, func(t *testing.T) {
			m := New(context.Background(), nil)
			defer m.Close()
			var id string
			var err error
			if tc.lsp {
				id, err = m.StartLSP("main", tc.command, t.TempDir(), 0, true, capture.OutputOptions{}, tc.label)
			} else {
				id, err = m.Start("main", tc.command, t.TempDir(), 0, true, true, capture.OutputOptions{}, tc.label)
			}
			if err != nil {
				t.Fatal(err)
			}
			// An inspector must never be needed to resolve stop identity.
			m.mu.Lock()
			m.jobs[id].inspector = func(Snapshot) InspectionSpec { panic("unexpected inspection") }
			m.mu.Unlock()
			v, err := m.Stop("main", id)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if want == "" {
				want = id
			}
			if v.StopLabel() != want || !utf8.ValidString(v.StopLabel()) || v.Stdout != "" || v.Stderr != "" || v.StdoutSelection != nil || v.StderrSelection != nil {
				t.Fatalf("stop=%+v identity=%q want=%q", v, v.StopLabel(), want)
			}
		})
	}
}

func TestStopWatchIdentity(t *testing.T) {
	for _, label := range []string{"", "Training", "External run"} {
		t.Run(label, func(t *testing.T) {
			dir, identity := watchFixture(t, false)
			m := New(context.Background(), nil)
			defer m.Close()
			id, err := m.StartWatch("main", dir, label, capture.OutputOptions{})
			if err != nil {
				t.Fatal(err)
			}
			v, err := m.Stop("main", id)
			if err != nil {
				t.Fatal(err)
			}
			want := label
			if want == "" {
				want = watchCommand(identity.Command)
			}
			if v.StopLabel() != want || v.Kind != "watch" {
				t.Fatalf("stop=%+v label=%q want=%q", v, v.StopLabel(), want)
			}
		})
	}
}

func TestStopTaskIdentityIgnoresAutomaticLabels(t *testing.T) {
	for _, tc := range []struct{ label, command, want string }{
		{},
		{label: " \t\n", command: " \t\n"},
		{command: " run\n checks ", want: "run checks"},
		{label: "Review", command: "run checks", want: "Review"},
	} {
		t.Run(tc.label+tc.command, func(t *testing.T) {
			m := New(context.Background(), nil)
			defer m.Close()
			id, err := m.StartTask("main", TaskOptions{Kind: "fixture", Label: tc.label, command: tc.command}, func(ctx context.Context, _, _ io.Writer) (TaskResult, error) {
				<-ctx.Done()
				return TaskResult{}, ctx.Err()
			})
			if err != nil {
				t.Fatal(err)
			}
			v, err := m.Stop("main", id)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if want == "" {
				want = id
			}
			if v.StopLabel() != want {
				t.Fatalf("label=%q want=%q (display=%q)", v.StopLabel(), want, v.Label)
			}
		})
	}
}

func TestStopGrepIdentityUsesID(t *testing.T) {
	m := New(context.Background(), nil)
	defer m.Close()
	id := grepFixture(t, m, "main", "hit one\n")
	page, err := m.Read(context.Background(), "main", id, ReadOptions{Grep: "hit", Limit: 1024})
	if err != nil {
		t.Fatal(err)
	}
	grepID := page["job_id"].(string)
	v, err := m.Stop("main", grepID)
	if err != nil {
		t.Fatal(err)
	}
	if v.StopLabel() != grepID || v.Kind != "grep" {
		t.Fatalf("stop=%+v label=%q", v, v.StopLabel())
	}
}
