package main

import (
	"context"
	"fmt"
	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"
	"os"

	"ttc/internal/history"
	"ttc/internal/rail"
)

func registerRailFlags(flags *flag.FlagSet, opts *rail.Options, data string) {
	flags.StringVar(&opts.Workdir, "workdir", ".", "workspace directory; attach or create its sandbox")
	flags.StringVar(&opts.DataDir, "data-dir", data, "existing TTC data root to share when creating a sandbox")
	flags.BoolVar(&opts.List, "list", false, "list live rail tmux sessions without attaching")
}

func validateRailData(flags *flag.FlagSet, opts rail.Options, dataDefaultErr error) error {
	// Inside tmux, attach/create becomes listing and needs no storage default.
	if !opts.List && os.Getenv("TMUX") == "" {
		if err := dataDefaultError(flags, dataDefaultErr); err != nil {
			return err
		}
	}
	return nil
}

// Environment defaults do not invalidate an explicit CLI storage choice.
func dataDefaultError(flags *flag.FlagSet, defaultErr error) error {
	if flags.Changed("data-dir") {
		return nil
	}
	return defaultErr
}

func newRailCommand() *cobra.Command {
	data, defaultErr := history.DataRoot()
	opts := rail.Options{Input: os.Stdin, Output: os.Stdout}
	cmd := &cobra.Command{Use: "rail", Short: "Launch or attach filesystem guardrails", Long: "Filesystem guardrails with shared host networking. Detach to keep running.", Args: cobra.NoArgs}
	registerRailFlags(cmd.Flags(), &opts, data)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := validateRailData(cmd.Flags(), opts, defaultErr); err != nil {
			return err
		}
		return launchRail(cmd.Context(), opts)
	}
	return cmd
}

func launchRail(ctx context.Context, opts rail.Options) error {
	if !opts.List && os.Getenv("TMUX") != "" {
		fmt.Fprintln(os.Stderr, "Warning: already inside tmux; not attaching or creating a rail instance. Listing live rail sessions; run from outside tmux to attach.")
		opts.List = true
	}
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
