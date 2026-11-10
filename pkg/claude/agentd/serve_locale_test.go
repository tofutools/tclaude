package agentd

import (
	"errors"
	"slices"
	"strings"
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

// A tmux server an earlier, locale-less agentd started keeps starting
// panes without a locale; agentd gives it its own at start.
func TestSyncTmuxServerLocale(t *testing.T) {
	for _, tc := range []struct {
		name    string
		environ []string
		server  string
		noSrv   bool
		want    []string
	}{
		{"server without locale", []string{"LANG=en_GB.UTF-8"}, "PATH=/usr/bin\n-LANG\n", false, []string{"set-environment -g LANG en_GB.UTF-8"}},
		{"LC_CTYPE carried as LC_CTYPE", []string{"LANG=C", "LC_CTYPE=en_GB.UTF-8"}, "PATH=/usr/bin\n", false, []string{"set-environment -g LC_CTYPE en_GB.UTF-8"}},
		{"server has its own", []string{"LANG=en_GB.UTF-8"}, "LANG=sv_SE.UTF-8\n", false, nil},
		{"no server", []string{"LANG=en_GB.UTF-8"}, "", true, nil},
		{"agentd has no UTF-8 locale", []string{"LANG=C"}, "PATH=/usr/bin\n", false, nil},
	} {
		var sets []string
		run := func(args ...string) ([]byte, error) {
			if args[0] == "show-environment" {
				if tc.noSrv {
					return nil, errors.New("no server running")
				}
				return []byte(tc.server), nil
			}
			sets = append(sets, strings.Join(args, " "))
			return nil, nil
		}
		syncTmuxServerLocale(tc.environ, run)
		if !slices.Equal(sets, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, sets, tc.want)
		}
	}
}
