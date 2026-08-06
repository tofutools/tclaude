package common

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// BootstrapShellFallback is what tclaude runs its generated bootstrap under
// when no bash can be found. It is the historical interpreter, so a host
// without bash keeps working exactly as it did — but only for the command text
// tclaude generates itself. Operator-authored shell must not run here.
const BootstrapShellFallback = "/bin/sh"

// bootstrapShellCandidates are tried in order before falling back to a PATH
// lookup. Absolute paths first: PATH inside a launched pane is the operator's,
// and a "bash" earlier on it is not necessarily the system shell.
// These two are variables only so a test can point the resolver at a fixture
// instead of the developer's real filesystem. Neither is reassigned in
// production.
var (
	bootstrapShellCandidates = []string{"/bin/bash", "/usr/bin/bash"}
	bootstrapShellLookPath   = exec.LookPath
)

var (
	bootstrapShellOnce     sync.Once
	bootstrapShellResolved string
)

// BootstrapShellPath is the interpreter tclaude uses for the shell text it
// composes for a pane: the launch script tmux starts, and the `-c` command the
// OS-sandbox wrapper executes inside the wall.
//
// It resolves to bash rather than `sh` because that text is not only tclaude's
// own. A sandbox profile's pre-launch blocks are operator-authored, and
// operators write bash — `/bin/sh` is dash on Debian/Ubuntu, where a bash-ism
// like `mkdir -p "$x"/{a,b}` does not fail, it silently creates one directory
// named `{a,b}`. Pinning the interpreter makes the vocabulary a property of
// tclaude rather than of whatever the host happens to link `sh` to.
//
// Inside the sandbox the same absolute path is reachable: the constructed root
// ro-binds /bin and /usr (tclaudeLayerStaticOSPaths), preserving merged-usr
// symlinks, so a host with bash yields a namespace with bash at the same path.
//
// The result is resolved once per process. Returns BootstrapShellFallback when
// no bash exists, which callers carrying operator-authored shell must refuse
// rather than accept — see BootstrapShellIsBash.
func BootstrapShellPath() string {
	bootstrapShellOnce.Do(func() {
		bootstrapShellResolved = resolveBootstrapShell()
	})
	return bootstrapShellResolved
}

// BootstrapShellIsBash reports whether the resolved interpreter is actually
// bash. A caller about to run operator-authored shell must refuse the launch
// when this is false instead of quietly running it under dash.
func BootstrapShellIsBash() bool {
	return BootstrapShellPath() != BootstrapShellFallback
}

func resolveBootstrapShell() string {
	for _, candidate := range bootstrapShellCandidates {
		if isExecutableFile(candidate) {
			return candidate
		}
	}
	if found, err := bootstrapShellLookPath("bash"); err == nil {
		if abs, absErr := filepath.Abs(found); absErr == nil && isExecutableFile(abs) {
			return abs
		}
	}
	slog.Warn("launch: no bash found; pane bootstrap falls back to /bin/sh",
		"candidates", bootstrapShellCandidates, "fallback", BootstrapShellFallback)
	return BootstrapShellFallback
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}

// IsBootstrapShellWord reports whether an argv word names an interpreter
// tclaude could have started its pane bootstrap with.
//
// Recognition is deliberately wider than BootstrapShellPath: a pane launched
// by an older tclaude carries the bare word `sh`, one launched on a host
// without bash carries `/bin/sh`, and the resolved bash path differs across
// hosts. Anything reading back a live pane's start command has to accept all
// of them or it stops recognizing panes it launched itself.
func IsBootstrapShellWord(word string) bool {
	switch filepath.Base(word) {
	case "sh", "bash":
		return true
	}
	return false
}
