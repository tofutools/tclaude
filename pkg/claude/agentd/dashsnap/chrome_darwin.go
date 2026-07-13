//go:build darwin

package dashsnap

import (
	"os"

	"github.com/go-rod/rod/lib/launcher"
)

const (
	macChromiumTmpDirEnv = "MAC_CHROMIUM_TMPDIR"
	platformLaunchHint   = " (macOS coding-agent sandboxes may deny Chrome's required Mach service registration; run this test outside the agent sandbox or on Linux)"
)

// configurePlatformChrome keeps Chromium's ProcessSingleton socket and related
// temporary state inside the disposable, known-writable browser directory.
// Chromium on macOS consults MAC_CHROMIUM_TMPDIR before
// _CS_DARWIN_USER_TEMP_DIR and does not follow the ordinary TMPDIR override for
// this state. Preserve an explicit caller setting.
func configurePlatformChrome(l *launcher.Launcher, browserDir string) {
	// Chromium treats an explicitly empty value as unset and falls back to
	// _CS_DARWIN_USER_TEMP_DIR, so replace empty as well as absent values.
	if os.Getenv(macChromiumTmpDirEnv) != "" {
		return
	}
	l.Env(append(os.Environ(), macChromiumTmpDirEnv+"="+browserDir)...)
}
