package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"scicode/internal/history"
	"scicode/internal/rail"
)

func railOptions(args []string) (rail.Options, error) {
	data, err := history.DataRoot()
	dataDefaultErr := err
	opts := rail.Options{Input: os.Stdin, Output: os.Stdout}
	flags := flag.NewFlagSet("ttc rail", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&opts.Workdir, "workdir", ".", "workspace directory; attach or create its sandbox")
	flags.StringVar(&opts.DataDir, "data-dir", data, "existing TTC data root to share when creating a sandbox")
	flags.BoolVar(&opts.List, "list", false, "list live rail tmux sessions without attaching")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: ttc rail [--workdir DIR] [--data-dir DIR] [--list]")
		fmt.Fprintln(flags.Output(), "Filesystem guardrails with shared host networking. Detach to keep running.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, fmt.Errorf("rail does not accept positional arguments: %v", flags.Args())
	}
	// Inside tmux, attach/create becomes listing and needs no storage default.
	if !opts.List && os.Getenv("TMUX") == "" {
		if err := dataDefaultError(flags, dataDefaultErr); err != nil {
			return opts, err
		}
	}
	return opts, nil
}

// Environment defaults do not invalidate an explicit CLI storage choice.
func dataDefaultError(flags *flag.FlagSet, defaultErr error) error {
	overridden := false
	flags.Visit(func(f *flag.Flag) { overridden = overridden || f.Name == "data-dir" })
	if overridden {
		return nil
	}
	return defaultErr
}

func runRail(args []string) error {
	opts, err := railOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if !opts.List && os.Getenv("TMUX") != "" {
		fmt.Fprintln(os.Stderr, "Warning: already inside tmux; not attaching or creating a rail instance. Listing live rail sessions; run from outside tmux to attach.")
		opts.List = true
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return rail.Run(ctx, opts)
}

// Only TTC drops agent credentials. Rail's launcher and private service
// processes must retain the host socket so interactive shells can forward it.
func applySSHAuthSockPolicy() error {
	socket, present := os.LookupEnv("SSH_AUTH_SOCK")
	if err := os.Unsetenv("SSH_AUTH_SOCK"); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	allowed, err := rail.AllowSSHAuthSock(home, os.Getenv("XDG_CONFIG_HOME"))
	if err != nil {
		return err
	}
	if allowed && present {
		return os.Setenv("SSH_AUTH_SOCK", socket)
	}
	return nil
}
