package launcher

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

type fakeRun struct {
	out   map[string]string
	calls []string
}

func (f *fakeRun) run(_ context.Context, name string, args ...string) ([]byte, error) {
	cmd := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	best, found := "", false
	for prefix := range f.out {
		if strings.HasPrefix(cmd, prefix) && (!found || len(prefix) > len(best)) {
			best, found = prefix, true
		}
	}
	if found {
		return []byte(f.out[best]), nil
	}
	return nil, errors.New("not found")
}

func testEnv(goos string, vars, files map[string]string) Env {
	return Env{
		GOOS: goos, PID: 4242, UID: 1000, EUID: 1000, Args: []string{"/bin/tclaude", "agentd", "serve"},
		Getenv: func(k string) string { return vars[k] },
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, os.ErrNotExist
		},
	}
}

const userShow = `Id=tclaude-agentd.service
MainPID=4242
ExecStart={ path=/home/u/.local/bin/tclaude ; argv[]=/home/u/.local/bin/tclaude agentd serve --no-tray --dashboard-port 7878 ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }
Environment=FOO=bar "SPACED=a b" API_TOKEN=hunter2
EnvironmentFiles=/home/u/.config/tclaude/agentd.env (ignore_errors=no)
Restart=on-failure
KillMode=process
FragmentPath=/home/u/.config/systemd/user/tclaude-agentd.service
`

func TestDetectSystemdUser(t *testing.T) {
	f := &fakeRun{out: map[string]string{"systemctl --user show tclaude-agentd.service": userShow}}
	env := testEnv("linux", map[string]string{"INVOCATION_ID": "abc"}, map[string]string{
		"/proc/4242/cgroup":                                   "0::/user.slice/user-1000.slice/user@1000.service/app.slice/tclaude-agentd.service\n",
		"/home/u/.config/tclaude/agentd.env":                  "# comment\nPATH=/usr/bin:/home/u/bin\nexport GITHUB_TOKEN=\"ghp_x\"\nPROXY=http://user:pw@proxy:8080\n",
		"/home/u/.config/systemd/user/tclaude-agentd.service": "# " + ManagedMarker + "\n[Service]\n",
	})
	info := Detect(context.Background(), env, f.run)
	if info.Kind != KindSystemdUser || info.Unit != "tclaude-agentd.service" {
		t.Fatalf("kind/unit = %s/%s", info.Kind, info.Unit)
	}
	wantArgv := []string{"/home/u/.local/bin/tclaude", "agentd", "serve", "--no-tray", "--dashboard-port", "7878"}
	if !reflect.DeepEqual(info.Argv, wantArgv) {
		t.Fatalf("argv = %q", info.Argv)
	}
	const envFile = "/home/u/.config/tclaude/agentd.env"
	wantEnv := []EnvVar{{"FOO", "bar", ""}, {"SPACED", "a b", ""}, {"API_TOKEN", Redacted, ""},
		{"PATH", "/usr/bin:/home/u/bin", envFile}, {"GITHUB_TOKEN", Redacted, envFile}, {"PROXY", Redacted, envFile}}
	if !reflect.DeepEqual(info.Env, wantEnv) {
		t.Fatalf("env = %+v", info.Env)
	}
	if !info.ManagedByTclaude || info.RestartMethod != RestartSupervisor || info.RestartPolicy != "on-failure" {
		t.Fatalf("info = %+v", info)
	}
	name, args, ok := RestartCommand(info)
	if !ok || name != "systemctl" || strings.Join(args, " ") != "--user --no-block restart tclaude-agentd.service" {
		t.Fatalf("restart = %v %s %q", ok, name, args)
	}
}

func TestDetectSystemdNotMainPIDIsNotSupervised(t *testing.T) {
	// A shell inside gnome-terminal-server.service inherits INVOCATION_ID and
	// the service's cgroup, but it is not the unit's main process.
	f := &fakeRun{out: map[string]string{"systemctl --user show": "Id=gnome-terminal-server.service\nMainPID=77\n"}}
	env := testEnv("linux", map[string]string{"INVOCATION_ID": "abc", "TMUX": "/tmp/tmux-1000/default,1,0"}, map[string]string{
		"/proc/4242/cgroup": "0::/user.slice/user-1000.slice/user@1000.service/app.slice/gnome-terminal-server.service\n",
	})
	info := Detect(context.Background(), env, f.run)
	if info.Kind != KindTmux || info.RestartMethod != RestartExec {
		t.Fatalf("info = %+v", info)
	}
	if _, _, ok := RestartCommand(info); ok {
		t.Fatal("unsupervised process must not get a supervisor restart command")
	}
}

func TestDetectSystemdKillModeControlGroupRestartsInPlace(t *testing.T) {
	show := strings.Replace(userShow, "KillMode=process", "KillMode=control-group", 1)
	show = strings.Replace(show, "user/tclaude-agentd", "system/tclaude-agentd", 1)
	f := &fakeRun{out: map[string]string{"systemctl show tclaude-agentd.service": show}}
	env := testEnv("linux", map[string]string{"INVOCATION_ID": "abc"}, map[string]string{
		"/proc/4242/cgroup": "0::/system.slice/tclaude-agentd.service\n",
	})
	info := Detect(context.Background(), env, f.run)
	if info.Kind != KindSystemdSystem || info.RestartMethod != RestartExec || len(info.Warnings) == 0 || info.ManagedByTclaude {
		t.Fatalf("info = %+v", info)
	}
}

func TestDetectSystemdSystemNonRootRestartMethod(t *testing.T) {
	for _, c := range []struct {
		name, props string
		euid        int
		method      string
		code        int
	}{
		{"on-failure exits 1", "", 1000, RestartExit, 1},
		{"1 is a success status", "SuccessExitStatus=1 SIGTERM\n", 1000, RestartExit, 2},
		{"Restart=no re-execs", "Restart=no\n", 1000, RestartExec, 0},
		{"on-abnormal re-execs", "Restart=on-abnormal\n", 1000, RestartExec, 0},
		{"root asks systemd", "", 0, RestartSupervisor, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Later keys win in parseShow, so props appended after override.
			f := &fakeRun{out: map[string]string{"systemctl show tclaude-agentd.service": userShow + c.props}}
			if strings.HasPrefix(c.props, "Restart=") {
				f.out["systemctl show tclaude-agentd.service"] = strings.Replace(userShow, "Restart=on-failure\n", c.props, 1)
			}
			env := testEnv("linux", map[string]string{"INVOCATION_ID": "abc"}, map[string]string{
				"/proc/4242/cgroup": "0::/system.slice/tclaude-agentd.service\n",
			})
			env.EUID = c.euid
			info := Detect(context.Background(), env, f.run)
			if info.Kind != KindSystemdSystem || info.RestartMethod != c.method || info.RestartExitCode != c.code {
				t.Fatalf("method/code = %s/%d, want %s/%d; warnings %q", info.RestartMethod, info.RestartExitCode, c.method, c.code, info.Warnings)
			}
			if c.method == RestartSupervisor {
				name, args, ok := RestartCommand(info)
				if !ok || name != "systemctl" || strings.Join(args, " ") != "--no-block restart tclaude-agentd.service" {
					t.Fatalf("root restart = %v %s %q", ok, name, args)
				}
			}
		})
	}
}

// A system unit with Restart=always and the default KillMode, whose
// ExecStart is a wrapper that execs agentd, with the tmux server in its own
// unit (a delegated tclaude-tmux.service): restarting by exit is safe and
// goes through the wrapper again.
const wrapperShow = `Id=tclaude-agentd.service
MainPID=4242
ExecStart={ path=/usr/bin/fish ; argv[]=/usr/bin/fish -lc exec /usr/local/bin/tclaude-agentd-serve ; ignore_errors=no }
Restart=always
KillMode=control-group
FragmentPath=/etc/systemd/system/tclaude-agentd.service
`

func TestDetectWrappedSystemUnitWithTmuxElsewhere(t *testing.T) {
	tmuxPID := 777
	files := map[string]string{
		"/proc/4242/cgroup":                          "0::/system.slice/tclaude-agentd.service\n",
		"/proc/777/cgroup":                           "0::/system.slice/tclaude-tmux.service/tclaude-tmux\n",
		"/etc/systemd/system/tclaude-agentd.service": "[Service]\n",
	}
	f := &fakeRun{out: map[string]string{"systemctl show tclaude-agentd.service": wrapperShow}}
	env := testEnv("linux", map[string]string{"INVOCATION_ID": "abc"}, files)
	env.Args = []string{"/home/tool/go/bin/tclaude", "agentd", "serve", "-p", "7878", "--dashboard-bind", "100.64.0.7"}
	env.TmuxServerPID = func() (int, error) { return tmuxPID, nil }

	info := Detect(context.Background(), env, f.run)
	if info.RestartMethod != RestartExit || info.RestartExitCode != 0 {
		t.Fatalf("method = %s/%d; warnings %q", info.RestartMethod, info.RestartExitCode, info.Warnings)
	}
	if info.Tmux == nil || !info.Tmux.Running || info.Tmux.InUnit || info.UnitCgroup != "/system.slice/tclaude-agentd.service" {
		t.Fatalf("tmux = %+v unit cgroup %q", info.Tmux, info.UnitCgroup)
	}
	if !info.Wrapped || info.Rewritable || info.ManagedByTclaude {
		t.Fatalf("wrapped/rewritable/managed = %v/%v/%v", info.Wrapped, info.Rewritable, info.ManagedByTclaude)
	}
	if !reflect.DeepEqual(info.RawArgv(), env.Args) {
		t.Fatalf("a wrapped unit's configured argv is the process's: %q", info.RawArgv())
	}

	// The same unit with tmux inside it (a delegated subgroup of agentd's
	// own unit): an exit would take the agents down, so re-exec in place.
	files["/proc/777/cgroup"] = "0::/system.slice/tclaude-agentd.service/tmux\n"
	if info := Detect(context.Background(), env, f.run); info.RestartMethod != RestartExec || info.Tmux == nil || !info.Tmux.InUnit {
		t.Fatalf("tmux in unit: method %s tmux %+v", info.RestartMethod, info.Tmux)
	}
	// No tmux server running: nothing to lose.
	tmuxPID = 0
	if info := Detect(context.Background(), env, f.run); info.RestartMethod != RestartExit {
		t.Fatalf("no tmux: method %s", info.RestartMethod)
	}
	// The server cannot be probed: KillMode alone decides, conservatively.
	env.TmuxServerPID = func() (int, error) { return 0, errors.New("unknown") }
	if info := Detect(context.Background(), env, f.run); info.RestartMethod != RestartExec || info.Tmux != nil {
		t.Fatalf("unknown tmux: method %s", info.RestartMethod)
	}
}

func TestUnitFromCgroupFindsDelegatedSubgroup(t *testing.T) {
	for raw, want := range map[string][3]string{
		"0::/system.slice/tclaude-agentd.service\n":                                {"tclaude-agentd.service", "/system.slice/tclaude-agentd.service", ""},
		"0::/system.slice/tclaude-agentd.service/agentd\n":                         {"tclaude-agentd.service", "/system.slice/tclaude-agentd.service", ""},
		"0::/user.slice/user-1000.slice/user@1000.service/app.slice/x.service\n":   {"x.service", "/user.slice/user-1000.slice/user@1000.service/app.slice/x.service", "user"},
		"0::/user.slice/user-1000.slice/user@1000.service/init.scope\n":            {"", "", ""},
		"12:pids:/whatever\n1:name=systemd:/system.slice/tclaude-agentd.service\n": {"tclaude-agentd.service", "/system.slice/tclaude-agentd.service", ""},
	} {
		unit, path, user := unitFromCgroup(raw)
		u := ""
		if user {
			u = "user"
		}
		if got := [3]string{unit, path, u}; got != want {
			t.Errorf("unitFromCgroup(%q) = %q, want %q", raw, got, want)
		}
	}
}

const launchctlOut = `gui/501/com.tofutools.tclaude.agentd = {
	active count = 1
	path = /Users/u/Library/LaunchAgents/com.tofutools.tclaude.agentd.plist
	type = LaunchAgent
	state = running

	program = /opt/homebrew/bin/tclaude
	arguments = {
		/opt/homebrew/bin/tclaude
		agentd
		serve
		--no-tray
	}

	inherited environment = {
		SSH_AUTH_SOCK => /private/tmp/x
	}

	default environment = {
		PATH => /usr/bin:/bin
	}

	environment = {
		PATH => /opt/homebrew/bin:/usr/bin
		OPENAI_API_KEY => sk-123
		XPC_SERVICE_NAME => com.tofutools.tclaude.agentd
	}

	domain = gui/501 [100005]
	pid = 4242
	immediate reason = speculative
}
`

func TestDetectLaunchd(t *testing.T) {
	f := &fakeRun{out: map[string]string{"launchctl print gui/501/com.tofutools.tclaude.agentd": launchctlOut}}
	env := testEnv("darwin", map[string]string{"XPC_SERVICE_NAME": "com.tofutools.tclaude.agentd"}, map[string]string{
		"/Users/u/Library/LaunchAgents/com.tofutools.tclaude.agentd.plist": "<!-- " + ManagedMarker + " -->",
	})
	env.UID = 501
	info := Detect(context.Background(), env, f.run)
	if info.Kind != KindLaunchd || info.Domain != "gui/501" || !info.ManagedByTclaude {
		t.Fatalf("info = %+v", info)
	}
	if !reflect.DeepEqual(info.Argv, []string{"/opt/homebrew/bin/tclaude", "agentd", "serve", "--no-tray"}) {
		t.Fatalf("argv = %q", info.Argv)
	}
	wantEnv := []EnvVar{{Key: "PATH", Value: "/opt/homebrew/bin:/usr/bin"}, {Key: "OPENAI_API_KEY", Value: Redacted}, {Key: "XPC_SERVICE_NAME", Value: "com.tofutools.tclaude.agentd"}}
	if !reflect.DeepEqual(info.Env, wantEnv) {
		t.Fatalf("env = %+v", info.Env)
	}
	name, args, ok := RestartCommand(info)
	if !ok || name != "launchctl" || strings.Join(args, " ") != "kickstart -k gui/501/com.tofutools.tclaude.agentd" {
		t.Fatalf("restart = %v %s %q", ok, name, args)
	}
}

func TestDetectLaunchdTerminalAppIsShell(t *testing.T) {
	f := &fakeRun{}
	env := testEnv("darwin", map[string]string{"XPC_SERVICE_NAME": "application.com.apple.Terminal.123", "TERM": "xterm", "SHLVL": "1"}, nil)
	info := Detect(context.Background(), env, f.run)
	if info.Kind != KindShell || len(f.calls) != 0 {
		t.Fatalf("info = %+v calls=%v", info, f.calls)
	}
}

func TestRestartCommandRejectsUnsafeNames(t *testing.T) {
	for _, info := range []Info{
		{Kind: KindSystemdUser, Unit: "x; rm -rf ~.service", RestartMethod: RestartSupervisor},
		{Kind: KindSystemdUser, Unit: "--x.service", RestartMethod: RestartSupervisor},
		{Kind: KindLaunchd, Label: "a/b", Domain: "gui/501", RestartMethod: RestartSupervisor},
		{Kind: KindLaunchd, Label: "ok", Domain: "gui/../x", RestartMethod: RestartSupervisor},
	} {
		if _, _, ok := RestartCommand(info); ok {
			t.Errorf("RestartCommand(%+v) accepted an unsafe name", info)
		}
	}
}

func TestRedactArgv(t *testing.T) {
	got := RedactArgv([]string{"/usr/bin/env", "HUB_TOKEN=abc", "PATH=/bin", "tclaude", "agentd", "serve",
		"--api-key", "k1", "--auth-token=k2", "--dashboard-port", "7878", "--no-tray",
		"sh", "-c", "SECRET=s1 exec tclaude --hub https://u:pw@hub.example"})
	want := []string{"/usr/bin/env", "HUB_TOKEN=" + Redacted, "PATH=/bin", "tclaude", "agentd", "serve",
		"--api-key", Redacted, "--auth-token=" + Redacted, "--dashboard-port", "7878", "--no-tray",
		"sh", "-c", "SECRET=" + Redacted + " exec tclaude --hub https://" + Redacted + "@hub.example"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// The unit's cgroup decides, not just tmux: an OpenCode server agentd
// started inside the unit would die with a unit restart too.
func TestRestartMethodCountsEveryProcessInTheUnit(t *testing.T) {
	files := map[string]string{
		"/proc/4242/cgroup": "0::/system.slice/tclaude-agentd.service\n",
		"/proc/777/cgroup":  "0::/system.slice/tclaude-tmux.service\n",
	}
	f := &fakeRun{out: map[string]string{"systemctl show tclaude-agentd.service": wrapperShow}}
	env := testEnv("linux", map[string]string{"INVOCATION_ID": "abc"}, files)
	env.TmuxServerPID = func() (int, error) { return 777, nil }
	procs := []int{4242, 5150}
	env.CgroupProcs = func(path string) ([]int, error) {
		if path != "/system.slice/tclaude-agentd.service" {
			t.Fatalf("read cgroup %q", path)
		}
		return procs, nil
	}
	info := Detect(context.Background(), env, f.run)
	if info.RestartMethod != RestartExec || info.UnitOtherPIDs == nil || !reflect.DeepEqual(*info.UnitOtherPIDs, []int{5150}) {
		t.Fatalf("other process in unit: method %s others %v", info.RestartMethod, info.UnitOtherPIDs)
	}
	procs = []int{4242}
	if info := Detect(context.Background(), env, f.run); info.RestartMethod != RestartExit {
		t.Fatalf("only agentd in unit: method %s", info.RestartMethod)
	}
	// The cgroup cannot be read: tmux placement decides.
	env.CgroupProcs = func(string) ([]int, error) { return nil, errors.New("no cgroupfs") }
	if info := Detect(context.Background(), env, f.run); info.RestartMethod != RestartExit || info.UnitOtherPIDs != nil {
		t.Fatalf("unreadable cgroup, tmux elsewhere: method %s", info.RestartMethod)
	}
}

func TestProcCgroupProcsWalksSubgroups(t *testing.T) {
	if _, err := os.Stat("/sys/fs/cgroup"); err != nil {
		t.Skip("no cgroupfs")
	}
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skip("no /proc")
	}
	pids, err := ProcCgroupProcs(cgroupPath(string(raw)))
	if err != nil {
		t.Skipf("cgroup not readable here: %v", err)
	}
	found := false
	for _, p := range pids {
		found = found || p == os.Getpid()
	}
	if !found {
		t.Fatalf("own pid %d not in own cgroup's procs %v", os.Getpid(), pids)
	}
}
