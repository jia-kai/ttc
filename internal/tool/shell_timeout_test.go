package tool

import (
	"context"
	"encoding/json"
	"testing"

	"ttc/internal/jobs"
)

func TestShellTimeoutAndWaitNormalization(t *testing.T) {
	r, w, _, _ := toolFixture(t)
	AddShell(r, nil, w, nil)
	shell, _ := r.Get("shell")
	for _, tc := range []struct {
		args          string
		timeout, wait *int
		background    bool
		warnings      int
	}{
		{args: `{"command":"true"}`},
		{args: `{"command":"true","timeout_seconds":0}`, timeout: intPointer(86400), warnings: 1},
		{args: `{"command":"true","timeout_seconds":-1}`, timeout: intPointer(1), warnings: 1},
		{args: `{"command":"true","timeout_seconds":86401}`, timeout: intPointer(86400), warnings: 1},
		{args: `{"command":"true","max_fg_wait_seconds":-1}`, wait: intPointer(0), warnings: 1},
		{args: `{"command":"true","max_fg_wait_seconds":86401}`, wait: intPointer(86400), warnings: 1},
		{args: `{"command":"true","max_fg_wait_seconds":0}`, wait: intPointer(0)},
		{args: `{"command":"true","background":true,"max_fg_wait_seconds":10}`, background: true, warnings: 1},
		{args: `{"command":"true","protocol":"lsp"}`, background: true, warnings: 1},
		{args: `{"command":"true","protocol":"lsp","max_fg_wait_seconds":0,"timeout_seconds":0}`, background: true, timeout: intPointer(86400), warnings: 3},
	} {
		t.Run(tc.args, func(t *testing.T) {
			call, err := shell.DecodeCall(1, []byte(tc.args))
			if err != nil {
				t.Fatal(err)
			}
			_, encoded, err := call.Encode()
			if err != nil {
				t.Fatal(err)
			}
			var a shellArgs
			if err = json.Unmarshal(encoded, &a); err != nil {
				t.Fatal(err)
			}
			same := func(a, b *int) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
			if !same(a.Timeout, tc.timeout) || !same(a.ForegroundWait, tc.wait) || a.Background != tc.background || len(call.InputWarnings()) != tc.warnings {
				t.Fatalf("effective %s warnings %+v", encoded, call.InputWarnings())
			}
		})
	}
	for _, args := range []string{`{"command":"true","timeout_ms":1}`, `{"command":"true","wake_on_exit":false}`} {
		if _, err := shell.DecodeCall(1, []byte(args)); err == nil {
			t.Fatalf("accepted obsolete input %s", args)
		}
	}
}

func intPointer(v int) *int { return &v }

func TestShellEffectiveDefaultsAndImmediatePromotion(t *testing.T) {
	r, w, x, req := toolFixture(t)
	m := jobs.New(context.Background(), nil)
	defer m.Close()
	AddShell(r, m, w, nil)
	for _, tc := range []struct {
		args                 string
		timeout              float64
		wait                 *float64
		background, promoted bool
	}{
		{args: `{"command":"true"}`, timeout: 3600, wait: floatPointer(10)},
		{args: `{"command":"sleep 30","max_fg_wait_seconds":0}`, timeout: 3600, wait: floatPointer(0), background: true, promoted: true},
		{args: `{"command":"sleep 30","background":true}`, timeout: 3600, background: true},
		{args: `{"command":"sleep 30","protocol":"lsp"}`, background: true},
	} {
		t.Run(tc.args, func(t *testing.T) {
			record := invoke(t, r, w, x, req, "shell", tc.args)
			var result jobs.Snapshot
			if err := json.Unmarshal(record.Result, &result); err != nil || result.ID == "" {
				t.Fatal(string(record.Result), err)
			}
			if tc.timeout == 0 {
				if result.TimeoutSeconds != nil {
					t.Fatal(result)
				}
			} else if result.TimeoutSeconds == nil || *result.TimeoutSeconds != tc.timeout {
				t.Fatal(result)
			}
			if (result.ForegroundWaitSeconds == nil) != (tc.wait == nil) || tc.wait != nil && *result.ForegroundWaitSeconds != *tc.wait || result.Background != tc.background || result.Promoted != tc.promoted {
				t.Fatal(result)
			}
			if result.Status == "running" {
				if _, err := m.Stop(x.Actor, result.ID); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func floatPointer(v float64) *float64 { return &v }
