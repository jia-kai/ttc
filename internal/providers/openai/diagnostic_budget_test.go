package openai

import (
	"strings"
	"testing"
)

func TestStreamFailureLimitsDiagnosticInspection(t *testing.T) {
	// The SSE parser permits large events, but error diagnostics must not parse
	// or sort megabytes of arbitrary object fields merely to print a few keys.
	raw := []byte(`{"type":"error","unknown":"` + strings.Repeat("PRIVATE_VALUE", failureStreamLimit) + `"}`)
	err := streamFailure("", raw, "error")
	assertFailureContains(t, err, "error", "event exceeds diagnostic inspection limit (64 KiB)")
	assertSafeFailure(t, err.Error(), "subscription stream terminated: ")
	if strings.Contains(err.Error(), "PRIVATE_VALUE") {
		t.Fatal("oversized unknown payload leaked", err)
	}
	allocations := testing.AllocsPerRun(10, func() { _ = streamFailure("", raw, "error") })
	if allocations > 20 {
		t.Fatalf("oversized event was parsed: %.0f allocations", allocations)
	}
}
