package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
