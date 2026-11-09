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
	for prefix, out := range f.out {
		if strings.HasPrefix(cmd, prefix) {
			return []byte(out), nil
		}
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
	wantEnv := []EnvVar{{"FOO", "bar"}, {"SPACED", "a b"}, {"API_TOKEN", Redacted},
		{"PATH", "/usr/bin:/home/u/bin"}, {"GITHUB_TOKEN", Redacted}, {"PROXY", Redacted}}
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

func TestDetectSystemdSystemNonRootRestartsInPlace(t *testing.T) {
	f := &fakeRun{out: map[string]string{"systemctl show tclaude-agentd.service": userShow}}
	env := testEnv("linux", map[string]string{"INVOCATION_ID": "abc"}, map[string]string{
		"/proc/4242/cgroup": "0::/system.slice/tclaude-agentd.service\n",
	})
	info := Detect(context.Background(), env, f.run)
	if info.Kind != KindSystemdSystem || info.RestartMethod != RestartExec {
		t.Fatalf("info = %+v", info)
	}
	env.EUID = 0
	info = Detect(context.Background(), env, f.run)
	name, args, ok := RestartCommand(info)
	if !ok || name != "systemctl" || strings.Join(args, " ") != "--no-block restart tclaude-agentd.service" {
		t.Fatalf("root restart = %v %s %q", ok, name, args)
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
	wantEnv := []EnvVar{{"PATH", "/opt/homebrew/bin:/usr/bin"}, {"OPENAI_API_KEY", Redacted}, {"XPC_SERVICE_NAME", "com.tofutools.tclaude.agentd"}}
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
