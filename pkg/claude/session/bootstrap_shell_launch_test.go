package session

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func requireHostBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("host has no bash; the /bin/sh fallback is the covered path here")
	}
	if !clcommon.BootstrapShellIsBash() {
		t.Skip("host bash is outside the sandbox OS surface; the fallback is the covered path here")
	}
}

// The interpreter that runs tclaude's generated bootstrap must be bash, not
// whatever the host links `sh` to. On Debian/Ubuntu `/bin/sh` is dash, where a
// bash-ism does not fail loudly — `mkdir -p "$x"/{a,b}` creates one directory
// literally named `{a,b}` — and the bootstrap is about to carry
// operator-authored pre-launch script blocks (TCL-1037).
func TestBootstrapShellIsBash(t *testing.T) {
	requireHostBash(t)
	shell := clcommon.BootstrapShellPath()
	assert.Equal(t, "bash", filepath.Base(shell))
	assert.True(t, filepath.IsAbs(shell), "a bare word would resolve against the launching PATH")
}

// Both shells that interpret the pane's command text — the one tmux starts on
// the script, and the one the OS-sandbox wrapper re-enters inside the wall —
// must be the same pinned interpreter. A profile's pre-launch block runs inside
// the wall, so pinning only the outer one would leave the operator's shell
// running under whatever `/bin/sh` is.
func TestSandboxExecShellPrefixUsesTheSamePinnedShell(t *testing.T) {
	prefix := sandboxExecShellPrefix()
	assert.Equal(t, " -- "+clcommon.BootstrapShellCommandPrefix()+" -c ", prefix)

	got, err := bwrapCommand("/usr/bin/bwrap", nil, nil, nil, nil, nil,
		sandboxpolicy.MountPlan{}, "exec agent")
	require.NoError(t, err)
	assert.Contains(t, got, prefix)

	if !clcommon.BootstrapShellIsBash() {
		return // the fallback IS /bin/sh; the assertion below would be circular
	}
	assert.NotContains(t, strings.TrimSuffix(got, clcommon.ShellQuoteArg("exec agent")),
		" -- /bin/sh -c ", "no wrapper may still hand the harness command to /bin/sh")
}

// The launch shell runs guardHarnessCommandWithDirProof — a fail-closed check
// built from `[ -f … ]`, `cd … && pwd -P` and `printf … > <ready>`. Under bash,
// $BASH_ENV and environment-exported functions can both reach into that, and a
// shadowed `pwd` or `printf` would defeat exactly the path-substitution and
// readiness checks the guard exists to enforce. `-p` closes both doors, so it
// must ride on every spelling of the interpreter.
func TestBootstrapShellCarriesPrivilegedFlag(t *testing.T) {
	requireHostBash(t)
	argv := clcommon.BootstrapShellArgv()
	require.Len(t, argv, 2)
	assert.Equal(t, clcommon.BootstrapShellPath(), argv[0])
	assert.Equal(t, "-p", argv[1])
	assert.Contains(t, sandboxExecShellPrefix(), " -p -c ")
}

// A PATH-resolved bash is only usable if it also exists INSIDE the sandbox. The
// isolated tclaude-layer posture builds its root from a fixed OS surface, so a
// bash outside that surface (NixOS's /nix/store/…, Linuxbrew's /home/…) would
// exec-fail the pane. This pins the two lists against each other rather than
// leaving the containment claim to a comment.
func TestBootstrapShellTrustedRootsAreInTheStaticOSSurface(t *testing.T) {
	surface := make(map[string]bool, len(tclaudeLayerStaticOSPaths))
	for _, path := range tclaudeLayerStaticOSPaths {
		surface[path] = true
	}
	for _, root := range clcommon.BootstrapShellTrustedRoots() {
		assert.True(t, surface[root],
			"%s accepts a bash the isolated posture would not bind into its root", root)
	}
}
