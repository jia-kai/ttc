package messagestream

import (
	"math/rand"
	"testing"
)

func TestDiffHashesReferenceLCS(t *testing.T) {
	rng := rand.New(rand.NewSource(17))
	for trial := 0; trial < 3000; trial++ {
		a, b := make([]string, rng.Intn(24)), make([]string, rng.Intn(24))
		for i := range a {
			a[i] = string(rune('A' + rng.Intn(5)))
		}
		for i := range b {
			b[i] = string(rune('A' + rng.Intn(5)))
		}
		matches, ok := DiffHashes(a, b)
		if !ok {
			t.Fatal("small diff exceeded budget")
		}
		dp := make([][]int, len(a)+1)
		for i := range dp {
			dp[i] = make([]int, len(b)+1)
		}
		for i := len(a) - 1; i >= 0; i-- {
			for j := len(b) - 1; j >= 0; j-- {
				if a[i] == b[j] {
					dp[i][j] = dp[i+1][j+1] + 1
				} else {
					dp[i][j] = max(dp[i+1][j], dp[i][j+1])
				}
			}
		}
		if len(matches) != dp[0][0] {
			t.Fatalf("a=%v b=%v matches=%v want length %d", a, b, matches, dp[0][0])
		}
		x, y := -1, -1
		for _, m := range matches {
			if m.Old <= x || m.Current <= y || a[m.Old] != b[m.Current] {
				t.Fatalf("invalid matches %v for %v / %v", matches, a, b)
			}
			x, y = m.Old, m.Current
		}
	}
}
func TestDiffHashesMiddleAndAppend(t *testing.T) {
	a := []string{"A", "B", "C", "D", "E", "F"}
	b := []string{"A", "B", "X", "D", "E", "F", "G"}
	matches, ok := DiffHashes(a, b)
	if !ok {
		t.Fatal("budget")
	}
	same := map[int]bool{}
	for _, m := range matches {
		same[m.Current] = true
	}
	for i := range b {
		if same[i] != (i != 2 && i != 6) {
			t.Fatal(matches)
		}
	}
}
func TestDiffHashesBudgetAndLargeAppend(t *testing.T) {
	a, b := make([]string, 10000), make([]string, 10000)
	for i := range a {
		a[i] = "A"
		b[i] = "B"
	}
	if _, ok := DiffHashes(a, b); ok {
		t.Fatal("unbounded disjoint diff")
	}
	b = append(append([]string{}, a...), "C")
	m, ok := DiffHashes(a, b)
	if !ok || len(m) != len(a) {
		t.Fatal(len(m), ok)
	}
}
