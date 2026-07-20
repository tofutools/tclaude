package agent

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunSpawn_AskHumanDoesNotClaimPendingPopupBeforeLineageDenial(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }
	DaemonRequestImpl = func(_, _ string, _, _ any, opts DaemonOpts) error {
		assert.Equal(t, time.Minute, opts.AskHuman)
		return &DaemonError{
			Status: http.StatusForbidden, Code: "approval_restricted",
			Msg: "approval lineage cannot be overridden by an authorization popup",
		}
	}

	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	resp, rc := RunSpawn(&SpawnParams{
		Group: "alpha", Name: "worker", Harness: "codex", AskHuman: "60s",
	}, stdout, stderr, new(bytes.Buffer))
	assert.Nil(t, resp)
	assert.NotEqual(t, rcOK, rc)
	assert.NotContains(t, stdout.String(), "Waiting", "no popup was known to be pending")
	assert.Contains(t, stdout.String(), "may be requested")
	assert.Contains(t, stderr.String(), "approval lineage")
}

// A --file brief over the 16384-byte cap is rejected with the same
// error as an oversize --initial-message: the file-input path enforces
// the cap, it is not a way to smuggle a larger brief past it. The
// rejection lands before the daemon is contacted, so this needs no
// running agentd.
func TestRunSpawn_FileBriefRejectedOverCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.txt")
	oversize := strings.Repeat("a", MaxInitialMessageBytes+1)
	require.NoError(t, os.WriteFile(path, []byte(oversize), 0o600))

	stderr := new(bytes.Buffer)
	resp, rc := RunSpawn(
		&SpawnParams{Group: "alpha", File: path},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	assert.Nil(t, resp)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "at most", "must surface the cap error")
}

// --initial-message and --file together is a usage error, surfaced
// before any spawn happens.
func TestRunSpawn_FileAndFlagMutuallyExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brief.txt")
	require.NoError(t, os.WriteFile(path, []byte("file brief"), 0o600))

	stderr := new(bytes.Buffer)
	resp, rc := RunSpawn(
		&SpawnParams{Group: "alpha", InitialMessage: "inline brief", File: path},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	assert.Nil(t, resp)
	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "not both")
}

// The spawn command's long help must state the default-resolution chain once,
// and the --harness flag help must warn that an unset value is NOT forced to
// claude — the TCL-304 documentation fix.
func TestSpawnHelp_DefaultResolutionDocumented(t *testing.T) {
	cmd := spawnCmd()
	long := cmd.Long
	assert.Contains(t, long, "Default resolution")
	assert.Contains(t, long, "group's default spawn profile")
	assert.Contains(t, long, "global (dashboard) default spawn profile")
	assert.Contains(t, long, "harness's own default")
	assert.Contains(t, long, "full chain FIRST")
	assert.Contains(t, long, "incompatible explicit")
	assert.Contains(t, long, "disclosed in the resolved-shape echo")
	assert.Contains(t, long, "tclaude agent profiles default show")

	harnessFlag := cmd.Flags().Lookup("harness")
	require.NotNil(t, harnessFlag)
	assert.Contains(t, harnessFlag.Usage, "never infer or pin")
	assert.Contains(t, harnessFlag.Usage, "--profile")

	approvalFlag := cmd.Flags().Lookup("ask-for-approval")
	require.NotNil(t, approvalFlag)
	assert.Contains(t, approvalFlag.Usage, "Claude: auto")
	assert.Contains(t, approvalFlag.Usage, "caller")
	assert.NotContains(t, approvalFlag.Usage, "Claude: inherit")
	assert.Contains(t, long, "narrowed from the harness default")
	assert.Contains(t, long, "never silently narrowed")
}

// The spawn command's long help must lead with the profile-first guidance —
// spawning with an operator-preconfigured spawn profile is the primary path,
// and the --profile flag help must say so too.
func TestSpawnHelp_ProfileFirstDocumented(t *testing.T) {
	cmd := spawnCmd()
	assert.True(t, strings.HasPrefix(cmd.Long, "Prefer a spawn profile"),
		"the profile-first guidance must open the long help, not trail it")
	assert.Contains(t, cmd.Long, "preconfigured by the operator")

	profileFlag := cmd.Flags().Lookup("profile")
	require.NotNil(t, profileFlag)
	assert.Contains(t, profileFlag.Usage, "RECOMMENDED")
	assert.Contains(t, profileFlag.Usage, "preconfigured by the operator")
}

// formatResolvedField renders "value (source)" for a pinned field and a bare
// "(harness default)" for an unpinned one.
func TestFormatResolvedField(t *testing.T) {
	assert.Equal(t, `codex (global default profile "x")`,
		formatResolvedField(ResolvedField{Value: "codex", Source: `global default profile "x"`}))
	assert.Equal(t, "(harness default)",
		formatResolvedField(ResolvedField{Value: "", Source: ProvHarnessDefault}))
	// A whitespace-only value is still treated as unpinned.
	assert.Equal(t, "(harness default)",
		formatResolvedField(ResolvedField{Value: "  ", Source: "explicit"}))
}

// A missing --file is rejected before the daemon is even contacted —
// nothing is spawned.
func TestRunSpawn_MissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.txt")

	stderr := new(bytes.Buffer)
	resp, rc := RunSpawn(
		&SpawnParams{Group: "alpha", File: missing},
		new(bytes.Buffer), stderr, new(bytes.Buffer),
	)
	assert.Nil(t, resp)
	assert.Equal(t, rcIOFailure, rc)
	assert.Contains(t, stderr.String(), missing, "error must name the unreadable file")
}
