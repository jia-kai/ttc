package rail

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBubblewrapInvocationTranslation(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nonexistent")
	plan := sandboxPlan{
		hostname: "chosen-host", workdir: "/work",
		namespaces: []namespaceKind{namespaceHostname, namespaceUser, namespaceProcess},
		newSession: true, dropCapabilities: true,
		filesystem: []filesystemOperation{
			{kind: filesystemProc, dest: "/proc"},
			{kind: filesystemDevices, dest: "/dev"},
			{kind: filesystemTmpfs, dest: "/tmp", writable: true},
			{kind: filesystemDirectory, dest: "/tmp/ttc", mode: fs.ModeSticky | 0777},
			{kind: filesystemDirectory, dest: "/tmp/ttc/1000", mode: 0700},
			{kind: filesystemSymlink, source: "../literal/./target", dest: "/alias"},
			{kind: filesystemBind, source: missing, dest: "/work", writable: true},
			{kind: filesystemBind, source: missing + "/config", dest: "/work/config"},
			{kind: filesystemBind, source: missing + "/mask", dest: "/work/config/secret"},
			{kind: filesystemTmpfs, dest: "/work/hidden"},
		},
		environment: []environmentChange{
			{name: "OVERRIDE", value: "planned"},
			{name: "REMOVE", unset: true},
			{name: "EMPTY"},
			{name: "OVERRIDE", unset: true},
			{name: "OVERRIDE", value: "last"},
		},
	}
	process := sandboxProcess{
		command:       []string{"/bin/tool", "", "--literal", "argument with spaces"},
		environment:   []string{"OVERRIDE=inherited", "REMOVE=yes", "UNRELATED=keep", "BASH_FUNC_f%%=() { :; }", "DUP=one", "DUP=two"},
		dieWithParent: true,
	}
	t.Setenv("OVERRIDE", "host-first")
	got, err := bubblewrapInvocation(plan, process)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OVERRIDE", "host-changed")
	t.Setenv("UNRELATED", "not-captured")
	again, err := bubblewrapInvocation(plan, process)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("host environment changed emission: %#v != %#v", got, again)
	}
	wantArgs := []string{
		"--unshare-uts", "--unshare-user", "--unshare-pid", "--hostname", "chosen-host",
		"--cap-drop", "ALL", "--new-session", "--die-with-parent",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		"--perms", "1777", "--dir", "/tmp/ttc", "--perms", "0700", "--dir", "/tmp/ttc/1000",
		"--symlink", "../literal/./target", "/alias",
		"--bind", missing, "/work", "--ro-bind", missing + "/config", "/work/config",
		"--ro-bind", missing + "/mask", "/work/config/secret",
		"--tmpfs", "/work/hidden", "--remount-ro", "/work/hidden",
		"--setenv", "OVERRIDE", "planned", "--unsetenv", "REMOVE", "--setenv", "EMPTY", "",
		"--unsetenv", "OVERRIDE", "--setenv", "OVERRIDE", "last",
		"--chdir", "/work", "--", "/bin/tool", "", "--literal", "argument with spaces",
	}
	if got.executable != "bwrap" || !reflect.DeepEqual(got.args, wantArgs) || !reflect.DeepEqual(got.environment, process.environment) {
		t.Fatalf("unexpected invocation:\n%#v\nwant args: %#v", got, wantArgs)
	}
}

func TestBubblewrapInvocationOwnership(t *testing.T) {
	plan := sandboxPlan{hostname: "host", workdir: "/work", environment: []environmentChange{{name: "NAME", value: "value"}}}
	process := sandboxProcess{command: []string{"tool", "arg"}, environment: []string{"NAME=original"}}
	got, err := bubblewrapInvocation(plan, process)
	if err != nil {
		t.Fatal(err)
	}
	process.command[0] = "changed-tool"
	process.environment[0] = "NAME=changed"
	plan.environment[0].value = "changed-value"
	if got.args[len(got.args)-2] != "tool" || got.environment[0] != "NAME=original" || got.args[4] != "value" {
		t.Fatalf("invocation aliases inputs: %#v", got)
	}
	got.environment[0] = "NAME=output-change"
	got.args[len(got.args)-1] = "output-change"
	if process.environment[0] != "NAME=changed" || process.command[1] != "arg" {
		t.Fatal("inputs alias invocation")
	}
}

func TestBubblewrapInvocationGeneratedFiles(t *testing.T) {
	plan := sandboxPlan{hostname: "host", workdir: "/work", filesystem: []filesystemOperation{
		{kind: filesystemBind, source: "/absent", dest: "/work"},
		{kind: filesystemFile, dest: "/work/config", data: "exact\x00\xffbytes"},
		{kind: filesystemFile, dest: "/work/hidden.conf"}, // A final empty file mask.
	}}
	got, err := bubblewrapInvocation(plan, sandboxProcess{command: []string{"tool"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--hostname", "host", "--ro-bind", "/absent", "/work", "--perms", "0600", "--ro-bind-data", "3", "/work/config", "--perms", "0600", "--ro-bind-data", "4", "/work/hidden.conf", "--chdir", "/work", "--", "tool"}
	if !reflect.DeepEqual(got.args, want) || !reflect.DeepEqual(got.files, []string{"exact\x00\xffbytes", ""}) {
		t.Fatalf("generated files changed or reordered: %#v", got)
	}
	plan.filesystem[1].data = "changed"
	if got.files[0] != "exact\x00\xffbytes" {
		t.Fatal("invocation aliases plan payloads")
	}
}

func TestBubblewrapInvocationExplicitChoices(t *testing.T) {
	plan := sandboxPlan{
		hostname: "host", workdir: "/",
		filesystem: []filesystemOperation{
			{kind: filesystemBind, source: "/does-not-exist", dest: "/proc", writable: true},
			{kind: filesystemDirectory, dest: "/zero", mode: 0},
			{kind: filesystemDirectory, dest: "/special", mode: fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky | 0750},
		},
	}
	got, err := bubblewrapInvocation(plan, sandboxProcess{command: []string{"relative-tool"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--hostname", "host", "--bind", "/does-not-exist", "/proc", "--perms", "0000", "--dir", "/zero", "--perms", "7750", "--dir", "/special", "--chdir", "/", "--", "relative-tool"}
	if !reflect.DeepEqual(got.args, want) {
		t.Fatalf("backend inferred choices or policy: got %#v, want %#v", got.args, want)
	}
}

func TestBubblewrapInvocationEmptyEnvironment(t *testing.T) {
	for _, environment := range [][]string{nil, {}} {
		got, err := bubblewrapInvocation(
			sandboxPlan{hostname: "host", workdir: "/"},
			sandboxProcess{command: []string{"tool"}, environment: environment},
		)
		if err != nil {
			t.Fatal(err)
		}
		if got.environment == nil || len(got.environment) != 0 {
			t.Fatalf("empty environment must not request live inheritance: %#v", got.environment)
		}
	}
}

func TestBubblewrapInvocationRejectsMalformedInputs(t *testing.T) {
	type malformedInput struct {
		name   string
		change func(*sandboxPlan, *sandboxProcess)
	}
	tests := []malformedInput{
		{"empty hostname", func(p *sandboxPlan, _ *sandboxProcess) { p.hostname = "" }},
		{"NUL hostname", func(p *sandboxPlan, _ *sandboxProcess) { p.hostname = "host\x00" }},
		{"empty workdir", func(p *sandboxPlan, _ *sandboxProcess) { p.workdir = "" }},
		{"relative workdir", func(p *sandboxPlan, _ *sandboxProcess) { p.workdir = "work" }},
		{"unclean workdir", func(p *sandboxPlan, _ *sandboxProcess) { p.workdir = "/work/../other" }},
		{"NUL workdir", func(p *sandboxPlan, _ *sandboxProcess) { p.workdir = "/work\x00" }},
		{"unknown namespace", func(p *sandboxPlan, _ *sandboxProcess) { p.namespaces = []namespaceKind{255} }},
		{"zero namespace", func(p *sandboxPlan, _ *sandboxProcess) { p.namespaces = []namespaceKind{0} }},
		{"duplicate namespace", func(p *sandboxPlan, _ *sandboxProcess) { p.namespaces = []namespaceKind{namespaceUser, namespaceUser} }},
		{"unknown filesystem kind", func(p *sandboxPlan, _ *sandboxProcess) { p.filesystem[0].kind = 255 }},
		{"zero filesystem kind", func(p *sandboxPlan, _ *sandboxProcess) { p.filesystem[0].kind = 0 }},
		{"empty destination", func(p *sandboxPlan, _ *sandboxProcess) { p.filesystem[0].dest = "" }},
		{"relative destination", func(p *sandboxPlan, _ *sandboxProcess) { p.filesystem[0].dest = "dest" }},
		{"unclean destination", func(p *sandboxPlan, _ *sandboxProcess) { p.filesystem[0].dest = "/dest/./child" }},
		{"NUL destination", func(p *sandboxPlan, _ *sandboxProcess) { p.filesystem[0].dest = "/dest\x00" }},
		{"empty bind source", func(p *sandboxPlan, _ *sandboxProcess) { p.filesystem[0].source = "" }},
		{"relative bind source", func(p *sandboxPlan, _ *sandboxProcess) { p.filesystem[0].source = "source" }},
		{"unclean bind source", func(p *sandboxPlan, _ *sandboxProcess) { p.filesystem[0].source = "/source/../other" }},
		{"NUL bind source", func(p *sandboxPlan, _ *sandboxProcess) { p.filesystem[0].source = "/source\x00" }},
		{"bind mode", func(p *sandboxPlan, _ *sandboxProcess) { p.filesystem[0].mode = 0700 }},
		{"bind data", func(p *sandboxPlan, _ *sandboxProcess) { p.filesystem[0].data = "unexpected" }},
		{"generated file source", func(p *sandboxPlan, _ *sandboxProcess) {
			p.filesystem[0] = filesystemOperation{kind: filesystemFile, dest: "/config", source: "/source"}
		}},
		{"empty symlink target", func(p *sandboxPlan, _ *sandboxProcess) {
			p.filesystem[0] = filesystemOperation{kind: filesystemSymlink, dest: "/link"}
		}},
		{"NUL symlink target", func(p *sandboxPlan, _ *sandboxProcess) {
			p.filesystem[0] = filesystemOperation{kind: filesystemSymlink, source: "target\x00", dest: "/link"}
		}},
		{"symlink writable", func(p *sandboxPlan, _ *sandboxProcess) {
			p.filesystem[0] = filesystemOperation{kind: filesystemSymlink, source: "target", dest: "/link", writable: true}
		}},
		{"directory type bits", func(p *sandboxPlan, _ *sandboxProcess) {
			p.filesystem[0] = filesystemOperation{kind: filesystemDirectory, dest: "/dir", mode: fs.ModeDir | 0700}
		}},
		{"directory raw sticky bit", func(p *sandboxPlan, _ *sandboxProcess) {
			p.filesystem[0] = filesystemOperation{kind: filesystemDirectory, dest: "/dir", mode: 01777}
		}},
		{"empty env name", func(p *sandboxPlan, _ *sandboxProcess) { p.environment = []environmentChange{{value: "value"}} }},
		{"equals env name", func(p *sandboxPlan, _ *sandboxProcess) { p.environment = []environmentChange{{name: "A=B"}} }},
		{"NUL env name", func(p *sandboxPlan, _ *sandboxProcess) { p.environment = []environmentChange{{name: "A\x00"}} }},
		{"NUL env value", func(p *sandboxPlan, _ *sandboxProcess) {
			p.environment = []environmentChange{{name: "A", value: "value\x00"}}
		}},
		{"unset with value", func(p *sandboxPlan, _ *sandboxProcess) {
			p.environment = []environmentChange{{name: "A", value: "value", unset: true}}
		}},
		{"missing command", func(_ *sandboxPlan, p *sandboxProcess) { p.command = nil }},
		{"empty executable", func(_ *sandboxPlan, p *sandboxProcess) { p.command = []string{""} }},
		{"NUL command", func(_ *sandboxPlan, p *sandboxProcess) { p.command = []string{"tool", "arg\x00"} }},
		{"inherited env missing equals", func(_ *sandboxPlan, p *sandboxProcess) { p.environment = []string{"NAME"} }},
		{"inherited env empty name", func(_ *sandboxPlan, p *sandboxProcess) { p.environment = []string{"=value"} }},
		{"inherited env NUL name", func(_ *sandboxPlan, p *sandboxProcess) { p.environment = []string{"NAME\x00=value"} }},
		{"inherited env NUL value", func(_ *sandboxPlan, p *sandboxProcess) { p.environment = []string{"NAME=value\x00"} }},
	}
	for _, kind := range []filesystemKind{filesystemProc, filesystemDevices, filesystemTmpfs, filesystemDirectory, filesystemFile} {
		for _, field := range []string{"source", "mode", "writable"} {
			if kind == filesystemDirectory && field == "mode" || kind == filesystemTmpfs && field == "writable" {
				continue
			}
			tests = append(tests, malformedInput{
				name: fmt.Sprintf("invalid %s for kind %d", field, kind),
				change: func(p *sandboxPlan, _ *sandboxProcess) {
					op := filesystemOperation{kind: kind, dest: "/dest"}
					switch field {
					case "source":
						op.source = "/source"
					case "mode":
						op.mode = 0700
					case "writable":
						op.writable = true
					}
					p.filesystem[0] = op
				},
			})
		}
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := sandboxPlan{hostname: "host", workdir: "/work", filesystem: []filesystemOperation{{kind: filesystemBind, source: "/nonexistent", dest: "/dest"}}}
			process := sandboxProcess{command: []string{"tool"}, environment: []string{"NAME=value"}}
			test.change(&plan, &process)
			got, err := bubblewrapInvocation(plan, process)
			if err == nil {
				t.Fatalf("accepted malformed input: %#v", got)
			}
			if got.executable != "" || got.args != nil || got.environment != nil {
				t.Fatalf("returned partial invocation on error: %#v", got)
			}
		})
	}
}
