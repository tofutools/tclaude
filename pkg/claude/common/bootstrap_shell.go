package common

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// BootstrapShellFallback is what tclaude runs its generated bootstrap under
// when no usable bash can be found. It is the historical interpreter, so a host
// without bash keeps working exactly as it did — but only for the command text
// tclaude generates itself. Operator-authored shell must not run here.
const BootstrapShellFallback = "/bin/sh"

// bootstrapShellPrivilegedFlag is bash's `-p`. tclaude is not setuid, so its
// privilege semantics are inert; what it buys is the part that matters for a
// launch authority: bash does not source $BASH_ENV, does not import functions
// exported through the environment, and ignores $SHELLOPTS.
//
// That is not a nicety. The bootstrap shell executes
// guardHarnessCommandWithDirProof, a fail-closed pre-exec check written in
// `[ -f … ]`, `"$(cd DIR && pwd -P)"` and `printf '%s' ok > <ready>`. dash could
// not be reached by either mechanism; bash can. An exported function shadowing
// `pwd` would make the canonicalization check pass unconditionally — exactly the
// path-substitution attack that guard exists to stop — and one shadowing
// `printf` would forge the readiness verdict the parent waits on. The same
// threat is already named in sandbox_stacked.go's probe environment: "Never
// inherit credentials, shell startup controls, or exported Bash functions that
// could spoof one of its fail-closed utility checks."
//
// Only passed when the resolved interpreter is bash: the fallback's option set
// is not tclaude's to assume.
const bootstrapShellPrivilegedFlag = "-p"

// These are variables only so a test can point the resolver at a fixture
// instead of the developer's real filesystem. None is reassigned in production.
var (
	bootstrapShellCandidates = []string{"/bin/bash", "/usr/bin/bash"}
	bootstrapShellLookPath   = exec.LookPath
	// bootstrapShellTrustedRoots bounds where a PATH-resolved bash may live.
	//
	// It MUST stay a subset of tclaudeLayerStaticOSPaths (the fixed OS surface
	// the isolated tclaude-layer posture binds into its constructed root); the
	// cross-package test TestBootstrapShellTrustedRootsAreInTheStaticOSSurface
	// pins that. A bash outside it — NixOS's /nix/store/…/bin/bash, Linuxbrew's
	// /home/linuxbrew/… — exists on the host but not inside that namespace, so
	// pinning it would exec-fail the pane the moment a profile asked for the
	// isolated posture. Falling back to /bin/sh is the honest answer there.
	bootstrapShellTrustedRoots = []string{"/bin", "/usr", "/sbin", "/opt"}
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
// ro-binds the static OS surface (/bin, /usr, …), preserving merged-usr
// symlinks, which is also why a PATH result outside that surface is refused.
//
// The result is resolved once per process. Returns BootstrapShellFallback when
// no usable bash exists, which callers carrying operator-authored shell must
// refuse rather than accept — see BootstrapShellIsBash.
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

// BootstrapShellArgv is the interpreter plus the options tclaude always wants
// on it, as exec-ready argv words. Use it wherever the shell is spawned with an
// argv (tmux's pane command, exec.Command); use BootstrapShellCommandPrefix
// where it has to be embedded in a shell-command string instead.
func BootstrapShellArgv() []string {
	shell := BootstrapShellPath()
	if !BootstrapShellIsBash() {
		return []string{shell}
	}
	return []string{shell, bootstrapShellPrivilegedFlag}
}

// BootstrapShellCommandPrefix is BootstrapShellArgv rendered as shell-quoted
// words, for embedding in a command string that another shell will parse.
func BootstrapShellCommandPrefix() string {
	words := BootstrapShellArgv()
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		quoted = append(quoted, ShellQuoteArg(word))
	}
	return strings.Join(quoted, " ")
}

// BootstrapShellTrustedRoots exposes the PATH-acceptance bound so the
// tclaude-layer package can assert it stays within the OS surface that posture
// actually binds. Callers must treat it as read-only.
func BootstrapShellTrustedRoots() []string {
	return append([]string(nil), bootstrapShellTrustedRoots...)
}

func resolveBootstrapShell() string {
	for _, candidate := range bootstrapShellCandidates {
		if isExecutableFile(candidate) {
			return candidate
		}
	}
	if found, err := bootstrapShellLookPath("bash"); err == nil {
		// A relative result (PATH carrying "." or a relative entry) is refused
		// rather than made absolute: filepath.Abs would resolve it against THIS
		// process's cwd, which is not the pane's cwd — tmux starts the pane with
		// its own `-c <cwd>`.
		switch {
		case !filepath.IsAbs(found):
			slog.Warn("launch: ignoring relative bash from PATH", "path", found)
		case !isExecutableFile(found):
		case !underBootstrapShellTrustedRoot(found):
			slog.Warn("launch: ignoring bash outside the sandbox OS surface; "+
				"it would not exist inside an isolated-posture namespace",
				"path", found, "trusted_roots", bootstrapShellTrustedRoots)
		default:
			return found
		}
	}
	slog.Warn("launch: no usable bash found; pane bootstrap falls back to /bin/sh",
		"candidates", bootstrapShellCandidates, "fallback", BootstrapShellFallback)
	return BootstrapShellFallback
}

func underBootstrapShellTrustedRoot(path string) bool {
	clean := filepath.Clean(path)
	for _, root := range bootstrapShellTrustedRoots {
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
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
