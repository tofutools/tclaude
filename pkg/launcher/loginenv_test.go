package launcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/testutil"
)

// TestMain lets the test binary stand in for `tclaude service launcher
// shell-env` inside a real login shell.
func TestMain(m *testing.M) {
	if os.Getenv("LAUNCHER_TEST_PRINT_SHELL_ENV") == "1" {
		if err := PrintShellEnv(os.Stdout); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestLoginShell(t *testing.T) {
	ctx := context.Background()
	fail := func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("no") }
	if got := LoginShell(ctx, "darwin", []string{"SHELL=/opt/homebrew/bin/fish"}, "u", fail); got != "/opt/homebrew/bin/fish" {
		t.Errorf("$SHELL: %s", got)
	}
	dscl := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "dscl" || strings.Join(args, " ") != ". -read /Users/u UserShell" {
			t.Errorf("ran %s %v", name, args)
		}
		return []byte("UserShell: /bin/bash\n"), nil
	}
	if got := LoginShell(ctx, "darwin", nil, "u", dscl); got != "/bin/bash" {
		t.Errorf("dscl: %s", got)
	}
	getent := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "getent" {
			t.Errorf("ran %s %v", name, args)
		}
		return []byte("u:x:1000:1000:U,,,:/home/u:/usr/bin/zsh\n"), nil
	}
	if got := LoginShell(ctx, "linux", []string{"SHELL="}, "u", getent); got != "/usr/bin/zsh" {
		t.Errorf("getent: %s", got)
	}
	if got := LoginShell(ctx, "darwin", nil, "u", fail); got != "/bin/zsh" {
		t.Errorf("darwin fallback: %s", got)
	}
	if got := LoginShell(ctx, "linux", []string{"SHELL=zsh"}, "u", fail); got != "/bin/sh" {
		t.Errorf("linux fallback: %s", got)
	}
}

func TestLoginShellCommand(t *testing.T) {
	argv, err := LoginShellCommand("/bin/zsh", []string{"/Users/u/bin/tclaude", "service", "launcher", "shell-env"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/bin/zsh", "-l", "-i", "-c", "exec '/Users/u/bin/tclaude' 'service' 'launcher' 'shell-env'"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv %q", argv)
	}
	for _, bad := range []string{"/x/it's/tclaude", `/x\y`, ""} {
		if _, err := LoginShellCommand("/bin/sh", []string{bad}); err == nil {
			t.Errorf("%q: quoted anyway", bad)
		}
	}
}

func TestParseShellEnv(t *testing.T) {
	env, err := ParseShellEnv([]byte("Welcome!\n" + ShellEnvMarker + `["A=1"]` + "\nnoise\n" + ShellEnvMarker + `["B=2","C=x=y","D=` + ShellEnvMarker + `[]"]` + "\n"))
	if err != nil || !slices.Equal(env, []string{"B=2", "C=x=y", "D=" + ShellEnvMarker + "[]"}) {
		t.Fatalf("%v %v", env, err)
	}
	for _, out := range []string{"", "no marker", "\n" + ShellEnvMarker + "[]", "\n" + ShellEnvMarker + "{", "x" + ShellEnvMarker + `["A=1"]`} {
		if _, err := ParseShellEnv([]byte(out)); err == nil {
			t.Errorf("%q parsed", out)
		}
	}
}

func TestMergeLoginEnv(t *testing.T) {
	base := []string{"PATH=/opt/brew/bin:/usr/bin", "TMPDIR=/var/folders/x", "LANG=C"}
	login := []string{"PATH=/Users/u/.local/bin:/usr/bin", "LANG=en_US.UTF-8", "ANTHROPIC_BASE_URL=http://proxy", "SHLVL=1", "PWD=/", "_=/usr/bin/tclaude"}
	got := MergeLoginEnv(base, login)
	want := []string{
		"PATH=/Users/u/.local/bin:/usr/bin:/opt/brew/bin",
		"TMPDIR=/var/folders/x",
		"LANG=en_US.UTF-8",
		"ANTHROPIC_BASE_URL=http://proxy",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// TestLoginShellEnvRunsTheProfile runs a real /bin/sh login shell, so the
// variables its ~/.profile exports reach the environment, and what the
// profile prints does not get in the way.
func TestLoginShellEnvRunsTheProfile(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("unix only")
	}
	home := testutil.CanonicalTempDir(t)
	profile := "echo welcome\nexport FROM_PROFILE=yes\nPATH=/opt/from-profile:$PATH\nexport PATH\n"
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv, err := LoginShellCommand("/bin/sh", []string{exe})
	if err != nil {
		t.Skip(err)
	}
	environ := []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LAUNCHER_TEST_PRINT_SHELL_ENV=1", "ENV="}
	env, err := LoginShellEnv(context.Background(), argv, environ)
	if err != nil {
		t.Fatal(err)
	}
	if v := envValue(env, "FROM_PROFILE"); v != "yes" {
		t.Errorf("FROM_PROFILE=%q in %q", v, env)
	}
	if v := envValue(env, "PATH"); !strings.HasPrefix(v, "/opt/from-profile:") {
		t.Errorf("PATH=%q", v)
	}

	// A shell that never reaches the dump is an error, not an empty env.
	argv[len(argv)-1] = "exit 0"
	if _, err := LoginShellEnv(context.Background(), argv, environ); err == nil {
		t.Error("no dump, no error")
	}
}

func TestWithDefaultLocale(t *testing.T) {
	if got := WithDefaultLocale([]string{"PATH=/usr/bin"}); !slices.Equal(got, []string{"PATH=/usr/bin", "LANG=en_US.UTF-8"}) {
		t.Errorf("none: %v", got)
	}
	for _, kv := range []string{"LANG=sv_SE.UTF-8", "LC_ALL=C.UTF-8", "LC_CTYPE=UTF-8"} {
		if got := WithDefaultLocale([]string{kv}); !slices.Equal(got, []string{kv}) {
			t.Errorf("%s: %v", kv, got)
		}
	}
}
