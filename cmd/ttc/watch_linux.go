package main

import (
	"errors"

	"github.com/spf13/cobra"
	"ttc/internal/watchrun"
)

// workloadExit carries the workload's exact exit status through Cobra.
type workloadExit struct{ code int }

func (e *workloadExit) Error() string { return "watch-run workload failed" }

func newWatchCommand() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "watch-run --record-dir DIR -- COMMAND [ARG...]",
		Short: "Run a command with durable output and completion records",
		Args: func(cmd *cobra.Command, args []string) error {
			if dir == "" {
				return errors.New("watch-run requires --record-dir")
			}
			if cmd.ArgsLenAtDash() != 0 {
				return errors.New("watch-run requires -- before the command")
			}
			if len(args) == 0 {
				return errors.New("watch-run requires a command after --")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			code, err := watchrun.Run(dir, args, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if code != 0 {
				return &workloadExit{code: code}
			}
			return nil
		},
	}
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().StringVar(&dir, "record-dir", "", "new, unique record directory (must not exist)")
	return cmd
}
