package providers

import (
	"context"
	"io"
	"testing"

	"github.com/spf13/pflag"
)

func shorthandModule(id, shorthand string) Module {
	return Module{ID: id, Configure: func(fs *pflag.FlagSet) (Factory, error) {
		fs.BoolP(id+"-enabled", shorthand, false, "fixture flag")
		return func(context.Context, Environment) (Components, error) { return Components{}, nil }, nil
	}}
}

func TestConfigureShorthandCollisionsAreAtomic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		modules []Module
	}{
		{"application", []Module{shorthandModule("first", "h")}},
		{"modules", []Module{shorthandModule("first", "x"), shorthandModule("second", "x")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
			fs.SetOutput(io.Discard)
			fs.BoolP("help", "h", false, "help")
			if configured, err := Configure(tc.modules, fs); err == nil || configured != nil {
				t.Fatalf("accepted collision: %v %v", configured, err)
			}
			if fs.NFlag() != 0 {
				t.Fatal("parsed state changed")
			}
			count := 0
			fs.VisitAll(func(*pflag.Flag) { count++ })
			if count != 1 || fs.ShorthandLookup("h").Name != "help" {
				t.Fatal("partial registration")
			}
		})
	}
}

func TestConfigurePreservesPflagMetadata(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	configured, err := Configure([]Module{shorthandModule("fixture", "x")}, fs)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse([]string{"-x"}); err != nil {
		t.Fatal(err)
	}
	if value, err := fs.GetBool("fixture-enabled"); err != nil || !value {
		t.Fatalf("boolean shorthand lost metadata: %v %v", value, err)
	}
	if _, err := Select(configured, "fixture"); err != nil {
		t.Fatal(err)
	}
}
