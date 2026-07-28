//go:build linux

package dashsnap

import (
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
)

func TestConfigurePlatformChromeIsolatesLinuxXDGState(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/ambient/config")
	t.Setenv("XDG_CACHE_HOME", "/ambient/cache")
	t.Setenv("XDG_DATA_HOME", "/ambient/data")
	t.Setenv("DASHSNAP_TEST_UNRELATED", "preserved")
	l := launcher.New()

	configurePlatformChrome(l, "/writable/dashsnap-browser")

	env, ok := l.GetFlags(flags.Env)
	if !ok {
		t.Fatal("launcher environment was not configured")
	}
	want := map[string]string{
		"XDG_CONFIG_HOME": "/writable/dashsnap-browser/config",
		"XDG_CACHE_HOME":  "/writable/dashsnap-browser/cache",
		"XDG_DATA_HOME":   "/writable/dashsnap-browser/data",
	}
	counts := make(map[string]int, len(want))
	unrelated := false
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		if expected, tracked := want[name]; tracked {
			counts[name]++
			if value != expected {
				t.Errorf("%s = %q, want %q", name, value, expected)
			}
		}
		if entry == "DASHSNAP_TEST_UNRELATED=preserved" {
			unrelated = true
		}
	}
	for name := range want {
		if counts[name] != 1 {
			t.Errorf("%s appeared %d times, want exactly once", name, counts[name])
		}
	}
	if !unrelated {
		t.Error("unrelated launcher environment entry was not preserved")
	}
}
