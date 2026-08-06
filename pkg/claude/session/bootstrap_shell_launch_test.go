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

// The interpreter that runs tclaude's generated bootstrap must be bash, not
// whatever the host links `sh` to. On Debian/Ubuntu `/bin/sh` is dash, where a
// bash-ism does not fail loudly — `mkdir -p "$x"/{a,b}` creates one directory
// literally named `{a,b}` — and the bootstrap is about to carry
// operator-authored pre-launch script blocks (TCL-1037).
//
// The whole point is that the vocabulary is tclaude's property rather than the
// host's, so this asserts bash specifically. It is skipped only on a host with
// no bash at all, which is the documented fallback rather than the contract.
func TestBootstrapShellIsBash(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("host has no bash; the /bin/sh fallback is the covered path here")
	}
	shell := clcommon.BootstrapShellPath()
	require.True(t, clcommon.BootstrapShellIsBash(), "resolved %q, wanted bash", shell)
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
	assert.Equal(t, " -- "+clcommon.ShellQuoteArg(clcommon.BootstrapShellPath())+" -c ", prefix)

	got, err := bwrapCommand("/usr/bin/bwrap", nil, nil, nil, nil, nil,
		sandboxpolicy.MountPlan{}, "exec agent")
	require.NoError(t, err)
	assert.Contains(t, got, prefix)
	assert.NotContains(t, strings.TrimSuffix(got, clcommon.ShellQuoteArg("exec agent")),
		" -- /bin/sh -c ", "no wrapper may still hand the harness command to /bin/sh")
}
