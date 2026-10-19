package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestMaintenanceProgressCountsCandidatesNotDeletedFamilies(t *testing.T) {
	s, first, _, _ := historyFixture(t)
	second, _ := startHistorySession(t, s, t.TempDir(), first.Model, "second family")
	if first.LineageID > second.LineageID {
		first, second = second, first
	}
	now := time.Now()
	for _, v := range []Session{first, second} {
		ageLineage(t, s, v.LineageID, now.Add(-31*24*time.Hour))
	}
	// Simulate a candidate becoming protected after discovery, within the same
	// transaction, without calling Store methods from a progress callback.
	if _, err := s.db.Exec(fmt.Sprintf(`CREATE TRIGGER refresh_second_family
		BEFORE DELETE ON sessions WHEN OLD.id='%s'
		BEGIN UPDATE sessions SET last_activity_ms=%d WHERE id='%s'; END`,
		first.ID, now.UnixMilli(), second.ID)); err != nil {
		t.Fatal(err)
	}
	var reports []MaintenanceProgress
	result, err := s.maintain(context.Background(), now, "", 1,
		func(p MaintenanceProgress) { reports = append(reports, p) })
	if err != nil || result.RemovedLineages != 1 {
		t.Fatal(result, err)
	}
	if _, err := s.Session(first.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("eligible first family survived", err)
	}
	if _, err := s.Session(second.ID); err != nil {
		t.Fatal("refreshed second family was deleted", err)
	}
	var phases []string
	lastCandidate := int64(-1)
	for _, p := range reports {
		phases = append(phases, p.Phase)
		if p.Phase == "candidates" {
			if p.Total != 2 || p.Completed < lastCandidate || p.Completed > p.Total {
				t.Fatalf("invalid phase-local candidate counts: %+v after %d", p, lastCandidate)
			}
			lastCandidate = p.Completed
		} else if p.Completed != 0 || p.Total != 0 {
			t.Fatalf("unknown-duration phase claimed determinate progress: %+v", p)
		}
	}
	if lastCandidate != 2 {
		t.Fatal("must process both candidates despite deleting only one", reports)
	}
	want := []string{"discovery", "candidates", "indexes", "candidates", "commit", "assets", "reclamation", "checkpoint", "vacuum", "checkpoint", "statistics"}
	if phases = slices.Compact(phases); !slices.Equal(phases, want) {
		t.Fatalf("phases %v, want %v", phases, want)
	}
}

func TestMaintenanceProgressCancellationAtCommitBoundary(t *testing.T) {
	for _, phase := range []string{"indexes", "assets"} {
		t.Run(phase, func(t *testing.T) {
			s, v, _, _ := historyFixture(t)
			now := time.Now()
			ageLineage(t, s, v.LineageID, now.Add(-31*24*time.Hour))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result, err := s.maintain(ctx, now, "", 1, func(p MaintenanceProgress) {
				if p.Phase == phase {
					cancel()
				}
			})
			want := 0
			if phase == "assets" {
				want = 1 // SQL is already committed even when asset cleanup fails.
			}
			if !errors.Is(err, context.Canceled) || result.RemovedLineages != want {
				t.Fatal("wrong committed deletion count", result, err)
			}
			_, err = s.Session(v.ID)
			if want == 0 && err != nil || want == 1 && !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("wrong SQL deletion state", err)
			}
			assertNoCleanupIndexes(t, s)
		})
	}
}
