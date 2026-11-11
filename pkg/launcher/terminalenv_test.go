package launcher

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestSessionSeedKeepsOnlyTheSession(t *testing.T) {
	environ := []string{"HOME=/Users/u", "USER=u", "SHELL=/opt/homebrew/bin/fish", "LANG=en_US.UTF-8", "LC_CTYPE=UTF-8",
		"GOROOT=/opt/homebrew/Cellar/go/1.27.3/libexec", "GOTOOLDIR=/x", "PATH=/opt/homebrew/Cellar/go/1.27.3/bin:/usr/bin", "API_KEY=k", "HTTPS_PROXY=http://p", "no_proxy=.corp", "XDG_CONFIG_HOME=/c"}
	got := SessionSeed("darwin", environ)
	want := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=/Users/u", "USER=u", "SHELL=/opt/homebrew/bin/fish", "LANG=en_US.UTF-8", "LC_CTYPE=UTF-8", "HTTPS_PROXY=http://p", "no_proxy=.corp", "XDG_CONFIG_HOME=/c"}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestMergeTerminalEnvDropsWhatTheShellDoesNotSet(t *testing.T) {
	seed := []string{"PATH=/usr/bin:/bin", "HOME=/h"}
	login := []string{"PATH=/opt/homebrew/bin:/usr/bin:/bin", "HOME=/h", "EDITOR=vi", "SHLVL=2"}
	environ := []string{"GOROOT=/stale", "PATH=/stale/bin:/usr/bin"}
	got := MergeTerminalEnv(seed, login, environ)
	want := []string{"HOME=/h", "EDITOR=vi", "PATH=/opt/homebrew/bin:/usr/bin:/bin:/stale/bin"}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// TestTerminalEnvAsksAFreshShell runs a real /bin/sh login shell from an
// environment holding a stale GOROOT the profile does not set: a new
// terminal would not have it, so the result does not either, while the
// profile's own variables arrive.
func TestTerminalEnvAsksAFreshShell(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("unix only")
	}
	home := testutil.CanonicalTempDir(t)
	// The test binary prints its environment only when told to; the
	// profile tells it, as nothing of environ but the session reaches it.
	profile := "export LAUNCHER_TEST_PRINT_SHELL_ENV=1\nexport FROM_PROFILE=yes\n"
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	environ := []string{"HOME=" + home, "SHELL=/bin/sh", "PATH=/stale/go/bin:/usr/bin:/bin", "GOROOT=/stale/go", "ENV="}
	env, err := TerminalEnv(context.Background(), runtime.GOOS, "", environ, []string{exe}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := envValue(env, "FROM_PROFILE"); v != "yes" {
		t.Errorf("FROM_PROFILE=%q in %q", v, env)
	}
	if v := envValue(env, "GOROOT"); v != "" {
		t.Errorf("stale GOROOT=%q reached the fresh environment", v)
	}
	if v := envValue(env, "PATH"); !strings.HasSuffix(v, ":/stale/go/bin") {
		t.Errorf("PATH=%q: agentd's PATH should only be a trailing fallback", v)
	}
}
