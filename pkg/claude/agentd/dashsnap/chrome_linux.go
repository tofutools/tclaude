//go:build linux

package dashsnap

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/go-rod/rod/lib/launcher"
)

const platformLaunchHint = ""

var linuxChromeXDGDirs = []struct {
	name string
	dir  string
}{
	{name: "XDG_CONFIG_HOME", dir: "config"},
	{name: "XDG_CACHE_HOME", dir: "cache"},
	{name: "XDG_DATA_HOME", dir: "data"},
}

// configurePlatformChrome keeps Chrome's process-global Linux state inside the
// disposable browser directory. In particular, crashpad does not follow
// --user-data-dir: it creates its database below XDG_CONFIG_HOME and aborts
// Chrome startup when the default ~/.config is read-only. Cache and data state
// are isolated alongside it so fontconfig, dconf, and NSS do not try to write
// elsewhere in the agent's home directory.
func configurePlatformChrome(l *launcher.Launcher, browserDir string) {
	overridden := make(map[string]bool, len(linuxChromeXDGDirs))
	for _, binding := range linuxChromeXDGDirs {
		overridden[binding.name] = true
	}

	env := make([]string, 0, len(os.Environ())+len(linuxChromeXDGDirs))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !overridden[name] {
			env = append(env, entry)
		}
	}
	for _, binding := range linuxChromeXDGDirs {
		env = append(env, binding.name+"="+filepath.Join(browserDir, binding.dir))
	}
	l.Env(env...)
}
