package launcher

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

type memFS map[string][]byte

func (m memFS) ReadFile(p string) ([]byte, error) {
	if b, ok := m[p]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}
func (m memFS) WriteFile(p string, b []byte, _ os.FileMode) error { m[p] = b; return nil }
func (m memFS) MkdirAll(string, os.FileMode) error                { return nil }
func (m memFS) Remove(p string) error {
	if _, ok := m[p]; !ok {
		return os.ErrNotExist
	}
	delete(m, p)
	return nil
}

var testSpec = AutostartSpec{
	Argv:          []string{"/home/u/.local/bin/tclaude", "agentd", "serve", "--dashboard-port", "7878", "--terminal", "my 100% $term"},
	Env:           []EnvVar{{Key: "PATH", Value: "/usr/bin:/home/u/bin"}, {Key: "API_TOKEN", Value: `s"ec\ret`}},
	ExtraEnvFiles: []string{"/etc/tclaude/extra.env"},
}

func TestSystemdAutostartRoundTrip(t *testing.T) {
	fs := memFS{}
	f := &fakeRun{out: map[string]string{"systemctl --user is-enabled": "enabled\n", "loginctl show-user u": "no\n", "systemctl --user": ""}}
	a := Autostart{GOOS: "linux", UID: 1000, User: "u", Paths: AutostartPaths("linux", "/home/u", ""), Run: f.run, FS: fs}
	if a.Paths.Unit != "/home/u/.config/systemd/user/tclaude-agentd.service" || a.Paths.EnvFile != "/home/u/.config/tclaude/agentd.env" {
		t.Fatalf("paths = %+v", a.Paths)
	}
	if err := a.Install(context.Background(), testSpec); err != nil {
		t.Fatal(err)
	}
	unit := string(fs[a.Paths.Unit])
	for _, want := range []string{ManagedMarker, "KillMode=process", "Restart=on-failure", "WantedBy=default.target",
		"EnvironmentFile=/home/u/.config/tclaude/agentd.env", "EnvironmentFile=-/etc/tclaude/extra.env",
		`ExecStart="/home/u/.local/bin/tclaude" "agentd" "serve" "--dashboard-port" "7878" "--terminal" "my 100%% $$term"`} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if env := string(fs[a.Paths.EnvFile]); !strings.Contains(env, `API_TOKEN="s\"ec\\ret"`) || !strings.Contains(env, `PATH="/usr/bin:/home/u/bin"`) {
		t.Fatalf("env file:\n%s", env)
	}
	if got := strings.Join(f.calls, "\n"); got != "systemctl --user daemon-reload\nsystemctl --user enable tclaude-agentd.service" {
		t.Fatalf("calls = %q", got)
	}
	st := a.Status(context.Background())
	if !st.Installed || !st.Managed || !st.Enabled || !reflect.DeepEqual(st.Argv, testSpec.Argv) {
		t.Fatalf("status = %+v", st)
	}
	if !reflect.DeepEqual(st.Env, []EnvVar{{Key: "API_TOKEN", Value: Redacted}, {Key: "PATH", Value: "/usr/bin:/home/u/bin"}}) {
		t.Fatalf("status env = %+v", st.Env)
	}
	if st.Linger == nil || st.Linger.Enabled || st.Linger.Command != "sudo loginctl enable-linger u" || len(st.Warnings) != 1 {
		t.Fatalf("linger = %+v warnings=%v", st.Linger, st.Warnings)
	}
	f.calls = nil
	if err := a.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fs) != 0 {
		t.Fatalf("uninstall left %v", fs)
	}
	if got := strings.Join(f.calls, "\n"); got != "systemctl --user disable tclaude-agentd.service\nsystemctl --user daemon-reload" {
		t.Fatalf("calls = %q", got)
	}
}

func TestAutostartRefusesUnitsItDidNotWrite(t *testing.T) {
	fs := memFS{}
	f := &fakeRun{out: map[string]string{"": ""}}
	a := Autostart{GOOS: "linux", User: "u", Paths: AutostartPaths("linux", "/home/u", "/home/u/xdg"), Run: f.run, FS: fs}
	fs[a.Paths.Unit] = []byte("[Service]\nExecStart=/mine\n")
	if err := a.Install(context.Background(), testSpec); !errors.Is(err, ErrNotManaged) {
		t.Fatalf("install over a hand-written unit: %v", err)
	}
	if err := a.Uninstall(context.Background()); !errors.Is(err, ErrNotManaged) {
		t.Fatalf("uninstall of a hand-written unit: %v", err)
	}
	delete(fs, a.Paths.Unit)
	fs[a.Paths.EnvFile] = []byte("PATH=/mine\n")
	if err := a.Install(context.Background(), testSpec); err == nil {
		t.Fatal("install overwrote a hand-written env file")
	}
	if string(fs[a.Paths.EnvFile]) != "PATH=/mine\n" || len(f.calls) != 0 {
		t.Fatalf("refused install still changed things: %q %v", fs[a.Paths.EnvFile], f.calls)
	}
}

func TestLaunchdAutostartRoundTrip(t *testing.T) {
	fs := memFS{}
	f := &fakeRun{out: map[string]string{"launchctl print-disabled gui/501": "disabled services = {\n\t\"com.tofutools.tclaude.agentd\" => enabled\n}\n", "launchctl": ""}}
	a := Autostart{GOOS: "darwin", UID: 501, Paths: AutostartPaths("darwin", "/Users/u", ""), Run: f.run, FS: fs}
	spec := testSpec
	spec.Argv = append([]string{}, spec.Argv...)
	spec.Argv[len(spec.Argv)-1] = "<x> & y"
	if err := a.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	plist := string(fs["/Users/u/Library/LaunchAgents/com.tofutools.tclaude.agentd.plist"])
	for _, want := range []string{ManagedMarker, "<key>AbandonProcessGroup</key>\n\t<true/>", "<string>&lt;x&gt; &amp; y</string>", "<key>SuccessfulExit</key>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	if got := strings.Join(f.calls, "\n"); got != "launchctl enable gui/501/com.tofutools.tclaude.agentd" {
		t.Fatalf("calls = %q", got)
	}
	st := a.Status(context.Background())
	if !st.Managed || !st.Enabled || !reflect.DeepEqual(st.Argv, spec.Argv) || st.Linger != nil {
		t.Fatalf("status = %+v", st)
	}
	if !reflect.DeepEqual(st.Env, []EnvVar{{Key: "API_TOKEN", Value: Redacted}, {Key: "PATH", Value: "/usr/bin:/home/u/bin"}}) {
		t.Fatalf("status env = %+v", st.Env)
	}
	if err := a.Uninstall(context.Background()); err != nil || len(fs) != 0 {
		t.Fatalf("uninstall: %v %v", err, fs)
	}
}

func TestValidateSpecRejectsUnitInjection(t *testing.T) {
	for _, spec := range []AutostartSpec{
		{Argv: []string{"tclaude", "agentd", "serve"}},
		{Argv: []string{"/bin/tclaude", "serve\n[Service]\nExecStartPre=/bin/evil"}},
		{Argv: []string{"/bin/tclaude", ""}},
		{Argv: []string{"/bin/tclaude"}, Env: []EnvVar{{Key: "A=B", Value: "x"}}},
		{Argv: []string{"/bin/tclaude"}, Env: []EnvVar{{Key: "A", Value: "x\nExecStartPre=/bin/evil"}}},
		{Argv: []string{"/bin/tclaude"}, ExtraEnvFiles: []string{"relative.env"}},
		{Argv: []string{"/bin/tclaude"}, ExtraEnvFiles: []string{"/a b.env"}},
	} {
		if err := ValidateSpec(spec); err == nil {
			t.Errorf("ValidateSpec(%+v) accepted it", spec)
		}
	}
}

func TestSystemdAutostartWithoutAUserManager(t *testing.T) {
	fs := memFS{}
	f := &fakeRun{} // every systemctl call fails
	a := Autostart{GOOS: "linux", User: "u", Paths: AutostartPaths("linux", "/home/u", ""), Run: f.run, FS: fs}
	if err := a.Install(context.Background(), testSpec); err == nil {
		t.Fatal("install succeeded without systemd")
	}
	if len(fs) != 0 {
		t.Fatalf("failed install left files: %v", fs)
	}
	fs[a.Paths.Unit] = []byte("# " + ManagedMarker + "\n")
	fs[a.Paths.EnvFile] = []byte("# " + ManagedMarker + "\n")
	if err := a.Uninstall(context.Background()); err != nil || len(fs) != 0 {
		t.Fatalf("uninstall without systemd: %v, left %v", err, fs)
	}
}

func TestAlreadyRunningExitCode(t *testing.T) {
	if AlreadyRunningExitCode("") != 75 || AlreadyRunningExitCode(LaunchdLabel) != 0 || AlreadyRunningExitCode("other") != 75 {
		t.Fatal("exit codes")
	}
	if !strings.Contains(string(RenderSystemdUnit(testSpec, "/e")), "RestartPreventExitStatus=75") {
		t.Fatal("unit does not stop on an already-running agentd")
	}
}
