//go:build darwin

package dashsnap

import (
	"os"
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
)

func TestConfigurePlatformChromeSetsWritableTempDir(t *testing.T) {
	old, hadOld := os.LookupEnv(macChromiumTmpDirEnv)
	if err := os.Unsetenv(macChromiumTmpDirEnv); err != nil {
		t.Fatalf("unset %s: %v", macChromiumTmpDirEnv, err)
	}
	t.Cleanup(func() {
		if hadOld {
			_ = os.Setenv(macChromiumTmpDirEnv, old)
		} else {
			_ = os.Unsetenv(macChromiumTmpDirEnv)
		}
	})
	l := launcher.New()

	configurePlatformChrome(l, "/writable/dashsnap-browser")

	env, ok := l.GetFlags(flags.Env)
	if !ok {
		t.Fatal("launcher environment was not configured")
	}
	want := macChromiumTmpDirEnv + "=/writable/dashsnap-browser"
	if got := env[len(env)-1]; got != want {
		t.Fatalf("last launcher environment entry = %q, want %q", got, want)
	}
	for _, entry := range env[:len(env)-1] {
		if strings.HasPrefix(entry, macChromiumTmpDirEnv+"=") {
			t.Fatalf("unexpected competing %s entry %q", macChromiumTmpDirEnv, entry)
		}
	}
}

func TestConfigurePlatformChromePreservesCallerSetting(t *testing.T) {
	t.Setenv(macChromiumTmpDirEnv, "/caller/chromium-tmp")
	l := launcher.New()

	configurePlatformChrome(l, "/writable/dashsnap-browser")

	if env, ok := l.GetFlags(flags.Env); ok {
		t.Fatalf("launcher environment unexpectedly replaced caller setting: %v", env)
	}
}

func TestConfigurePlatformChromePreservesExplicitEmptySetting(t *testing.T) {
	t.Setenv(macChromiumTmpDirEnv, "")
	l := launcher.New()

	configurePlatformChrome(l, "/writable/dashsnap-browser")

	if env, ok := l.GetFlags(flags.Env); ok {
		t.Fatalf("launcher environment unexpectedly replaced caller setting: %v", env)
	}
}
