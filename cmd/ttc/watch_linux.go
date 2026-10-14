package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"ttc/internal/watchrun"
)

func runWatch(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	flags := flag.NewFlagSet("watch-run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("record-dir", "", "new, unique record directory (must not exist)")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ttc watch-run --record-dir DIR -- COMMAND [ARG...]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 1, err
	}
	if *dir == "" {
		return 1, errors.New("watch-run requires --record-dir")
	}
	// Inspect only the wrapper prefix consumed by flag.Parse. A separator in
	// the workload arguments is not a wrapper boundary, and record-dir's value
	// may itself be "--". Parse above still owns flag/help/error semantics.
	separator := false
	for i, consumed := 0, len(args)-flags.NArg(); i < consumed; i++ {
		if args[i] == "--" {
			separator = true
			break
		}
		if args[i] == "--record-dir" || args[i] == "-record-dir" {
			i++ // Skip the separate value of the wrapper's string flag.
		}
	}
	if !separator {
		return 1, errors.New("watch-run requires -- before the command")
	}
	return watchrun.Run(*dir, flags.Args(), stdin, stdout, stderr)
}

func watchMain() {
	code, err := runWatch(os.Args[2:], os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ttc watch-run:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
