package managed

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestScopeCancelsAllBeforeJoiningAndSealsRegistration(t *testing.T) {
	var scope Scope
	first, second := make(chan struct{}), make(chan struct{})
	var cancelled atomic.Int32
	cancel := func() {
		if cancelled.Add(1) == 2 {
			close(first)
			close(second)
		}
	}
	if !scope.Register(cancel, first) {
		t.Fatal("registration rejected")
	}
	if !scope.Register(cancel, second) {
		t.Fatal("registration rejected")
	}
	scope.Close()
	if scope.Register(func() {}, make(chan struct{})) {
		t.Fatal("closed scope accepted work")
	}
}

func TestScopeRegistrationRace(t *testing.T) {
	for range 100 {
		var scope Scope
		done := make(chan struct{})
		var once sync.Once
		cancel := func() { once.Do(func() { close(done) }) }
		start := make(chan struct{})
		joined := make(chan struct{})
		go func() { <-start; scope.Close(); close(joined) }()
		close(start)
		if !scope.Register(cancel, done) {
			cancel()
		}
		<-joined
		<-done
	}
}

func TestScopeReleasesCompletedRegistration(t *testing.T) {
	var scope Scope
	done := make(chan struct{})
	if !scope.Register(func() { t.Error("completed work was cancelled") }, done) {
		t.Fatal("registration rejected")
	}
	close(done)
	scope.Release(done)
	if len(scope.members) != 0 {
		t.Fatal("completed closure retained")
	}
	if !scope.Register(func() { t.Error("already completed work was cancelled") }, done) {
		t.Fatal("completed registration rejected")
	}
	if len(scope.members) != 0 {
		t.Fatal("already completed fence retained")
	}
	scope.Close()
}
