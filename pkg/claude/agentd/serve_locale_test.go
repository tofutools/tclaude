package agentd

import (
	"slices"
	"testing"

	"github.com/tofutools/tclaude/pkg/launcher"
)

// agentd started without a locale (launchd, systemd, an app launcher) sets
// the user's own UTF-8 locale for itself and its children; one it was
// given is left alone.
func TestEnsureUTF8Locale(t *testing.T) {
	src := launcher.LocaleSources{
		GOOS:        "darwin",
		AppleLocale: func() (string, error) { return "en_GB@rg=sezzzz", nil },
		Installed:   func() ([]string, error) { return []string{"en_GB.UTF-8", "en_US.UTF-8"}, nil },
	}
	for _, tc := range []struct {
		env  []string
		want []string
	}{
		{[]string{"PATH=/usr/bin"}, []string{"LANG=en_GB.UTF-8"}},
		{[]string{"LANG=C"}, []string{"LC_CTYPE=en_GB.UTF-8"}},
		{[]string{"LANG=sv_SE.UTF-8"}, nil},
	} {
		var set []string
		ensureUTF8Locale(localeDeps{Environ: tc.env, Sources: src, Setenv: func(k, v string) error {
			set = append(set, k+"="+v)
			return nil
		}})
		if !slices.Equal(set, tc.want) {
			t.Errorf("%v: set %v, want %v", tc.env, set, tc.want)
		}
	}
}
