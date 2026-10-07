package providers

import (
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"

	"ttc/internal/llm"
)

func fakeModule(id, name string) Module {
	return Module{ID: id, Configure: func(fs *flag.FlagSet) (Factory, error) {
		value := fs.String(name, "default", "fake setting")
		return func(context.Context, Environment) (Components, error) {
			return Components{Inference: &llm.Script{}, Notice: *value}, nil
		}, nil
	}}
}

func TestNeutralModuleConfigurationAndSelection(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	configured, err := Configure([]Module{fakeModule("first", "first-setting"), fakeModule("second", "second-setting")}, fs)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse([]string{"--second-setting", "selected"}); err != nil {
		t.Fatal(err)
	}
	factory, err := Select(configured, "second")
	if err != nil {
		t.Fatal(err)
	}
	components, err := factory(context.Background(), Environment{})
	if err != nil || components.Inference == nil || components.Notice != "selected" || components.Catalog != nil || components.Authorize != nil {
		t.Fatal(components, err)
	}
	first, err := configured[0].Open(context.Background(), Environment{})
	if err != nil || first.Notice != "default" {
		t.Fatal(first, err)
	}
	// A second Configure call must not share the first factories' flag storage.
	another, err := Configure([]Module{fakeModule("second", "second-setting")}, flag.NewFlagSet("other", flag.ContinueOnError))
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := another[0].Open(context.Background(), Environment{})
	if err != nil || fresh.Notice != "default" {
		t.Fatal(fresh, err)
	}
}

func TestConfigureRejectsInvalidModulesAtomically(t *testing.T) {
	badFactory := Module{ID: "bad", Configure: func(*flag.FlagSet) (Factory, error) { return nil, nil }}
	badConfig := Module{ID: "bad", Configure: func(fs *flag.FlagSet) (Factory, error) {
		fs.String("staged", "", "")
		return nil, errors.New("configuration failed")
	}}
	duplicateFlag := Module{ID: "bad", Configure: func(fs *flag.FlagSet) (Factory, error) {
		fs.String("same", "", "")
		fs.String("same", "", "")
		return nil, nil
	}}
	invalidFlag := Module{ID: "bad", Configure: func(fs *flag.FlagSet) (Factory, error) {
		fs.String("-invalid", "", "")
		return nil, nil
	}}
	cases := []struct {
		name    string
		modules []Module
	}{
		{"empty", nil},
		{"empty ID", []Module{fakeModule("", "setting")}},
		{"whitespace ID", []Module{fakeModule(" ", "setting")}},
		{"duplicate ID", []Module{fakeModule("same", "one"), fakeModule("same", "two")}},
		{"missing configure", []Module{{ID: "bad"}}},
		{"missing factory", []Module{badFactory}},
		{"configuration error", []Module{fakeModule("first", "first"), badConfig}},
		{"application collision", []Module{fakeModule("first", "existing")}},
		{"module collision", []Module{fakeModule("first", "same"), fakeModule("second", "same")}},
		{"duplicate declaration", []Module{duplicateFlag}},
		{"invalid declaration", []Module{invalidFlag}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			existing := fs.String("existing", "preserved", "")
			configured, err := Configure(tc.modules, fs)
			if err == nil || configured != nil {
				t.Fatal(configured, err)
			}
			count := 0
			fs.VisitAll(func(*flag.Flag) { count++ })
			if count != 1 || *existing != "preserved" {
				t.Fatal("partial registration", count, *existing)
			}
		})
	}
	if _, err := Configure([]Module{fakeModule("valid", "setting")}, nil); err == nil {
		t.Fatal("accepted nil flag set")
	}
}

func TestSelectRejectsInvalidConfiguration(t *testing.T) {
	factory := Factory(func(context.Context, Environment) (Components, error) {
		return Components{Inference: &llm.Script{}}, nil
	})
	cases := []struct {
		configured []Configured
		id         string
	}{
		{nil, "any"},
		{[]Configured{{ID: "one", Open: factory}}, "unknown"},
		{[]Configured{{ID: "one", Open: factory}}, ""},
		{[]Configured{{ID: "", Open: factory}}, ""},
		{[]Configured{{ID: "one"}}, "one"},
		{[]Configured{{ID: "one", Open: factory}, {ID: "one", Open: factory}}, "one"},
		{[]Configured{{ID: "one", Open: factory}, {ID: "bad"}}, "one"},
	}
	for _, tc := range cases {
		if selected, err := Select(tc.configured, tc.id); err == nil || selected != nil {
			t.Fatal("accepted invalid configuration", err)
		}
	}
}

func TestProviderIDsUseChoiceValidation(t *testing.T) {
	factory := Factory(func(context.Context, Environment) (Components, error) { return Components{}, nil })
	for _, id := range []string{"", " ", " padded", "padded ", "control\ncharacter", "control\x00character", "invalid\xff", strings.Repeat("p", llm.MaxProviderIDBytes+1)} {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		if _, err := Configure([]Module{fakeModule(id, "setting")}, fs); err == nil {
			t.Errorf("Configure accepted ID %q", id)
		}
		if _, err := Select([]Configured{{ID: id, Open: factory}}, "valid"); err == nil {
			t.Errorf("Select accepted configured ID %q", id)
		}
		if _, err := Select([]Configured{{ID: "valid", Open: factory}}, id); err == nil {
			t.Errorf("Select accepted requested ID %q", id)
		}
	}
	id := strings.Repeat("p", llm.MaxProviderIDBytes)
	configured, err := Configure([]Module{fakeModule(id, "setting")}, flag.NewFlagSet("test", flag.ContinueOnError))
	if err != nil {
		t.Fatal("rejected maximum-length ID", err)
	}
	if _, err := Select(configured, id); err != nil {
		t.Fatal(err)
	}
}

func TestSelectEnforcesExplicitFlagOwnership(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantError bool
	}{
		{"defaults", nil, false},
		{"application flag", []string{"--application", "app"}, false},
		{"selected provider flag", []string{"--first-setting", "configured"}, false},
		{"nonselected provider flag", []string{"--second-setting", "configured"}, true},
		{"explicit default", []string{"--second-setting", "default"}, true},
		{"both providers", []string{"--first-setting", "one", "--second-setting", "two"}, true},
		{"explicit false", []string{"--second-enabled=false"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.String("application", "", "")
			second := fakeModule("second", "second-setting")
			configure := second.Configure
			second.Configure = func(staged *flag.FlagSet) (Factory, error) {
				staged.Bool("second-enabled", false, "")
				return configure(staged)
			}
			configured, err := Configure([]Module{fakeModule("first", "first-setting"), second}, fs)
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			factory, err := Select(configured, "first")
			if tc.wantError {
				if err == nil || factory != nil || !strings.Contains(err.Error(), "belongs to provider") {
					t.Fatal("accepted nonselected flag", err)
				}
			} else if err != nil || factory == nil {
				t.Fatal(factory, err)
			}
		})
	}
}
