package main

import (
	"errors"
	"flag"
	"testing"
)

func TestRailOptions(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TTC_DATA_DIR", "")
	t.Setenv("XDG_DATA_HOME", "/fixture/data")
	opts, err := railOptions(nil)
	if err != nil || opts.Workdir != "." || opts.DataDir != "/fixture/data/ttc" || opts.List {
		t.Fatalf("defaults: %+v %v", opts, err)
	}
	opts, err = railOptions([]string{"--workdir", "/project", "--data-dir", "/state", "--list"})
	if err != nil || opts.Workdir != "/project" || opts.DataDir != "/state" || !opts.List {
		t.Fatalf("flags: %+v %v", opts, err)
	}
	for _, args := range [][]string{{"--unknown"}, {"extra"}, {"--workdir"}} {
		if _, err := railOptions(args); err == nil {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
	if _, err := railOptions([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatal("help handling", err)
	}
}

func TestRailDataDefaultOverride(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TTC_DATA_DIR", "relative")
	if _, err := railOptions(nil); err == nil {
		t.Fatal("invalid environment default accepted without an override")
	}
	if opts, err := railOptions([]string{"--data-dir", "/explicit"}); err != nil || opts.DataDir != "/explicit" {
		t.Fatalf("explicit data root did not override invalid default: %+v %v", opts, err)
	}
	if _, err := railOptions([]string{"--list"}); err != nil {
		t.Fatal("listing should not require a data root", err)
	}
	if _, err := railOptions([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatal("invalid storage environment blocked help", err)
	}
	t.Setenv("TMUX", "/tmp/tmux-fixture,123,0")
	if _, err := railOptions(nil); err != nil {
		t.Fatal("listing inside tmux should not require a data root", err)
	}
}
