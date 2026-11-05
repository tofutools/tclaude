package agentd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// These pin what startOpenCodeRuntime decides before and around its server
// launch, without a real OpenCode server: the launch-input refusals, the
// refusals that keep an existing runtime row, the retirements of a stale or
// label-keyed prior runtime, and the port-attempt failure. The healthy-reuse
// branch needs a server that passes the managed-listener PID proof; it is
// covered at the pieces level by the reconcile and permission tests.

const openCodePinnedPermission = `[{"permission":"*","pattern":"*","action":"allow"}]`

// openCodeStartPin isolates the DB and puts only the given `opencode` (or
// none) on PATH, so the launch fails at a known point.
func openCodeStartPin(t *testing.T, opencodeScript string) (cwd string) {
	t.Helper()
	setupTestDB(t)
	bin := testutil.CanonicalTempDir(t)
	if opencodeScript != "" {
		require.NoError(t, os.WriteFile(filepath.Join(bin, "opencode"), []byte(opencodeScript), 0o755))
	}
	t.Setenv("PATH", bin)
	// Launch the server directly, as CI does, not through an agent's
	// delegated tmux.
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Setenv("TMUX", "")
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	return testutil.CanonicalTempDir(t)
}

func startPinnedOpenCodeRuntime(sessionID, cwd, resumeID, permissionJSON string) (*openCodeLaunch, error) {
	return startOpenCodeRuntime(sessionID, cwd, "pin title", resumeID, permissionJSON,
		string(sandboxpolicy.ImplementationOff), nil, "")
}

func TestStartOpenCodeRuntime_RefusesMissingOrInvalidPermission(t *testing.T) {
	cwd := openCodeStartPin(t, "")
	for _, tc := range []struct{ name, permission, refusal string }{
		{"missing", "  ", "OpenCode permission policy is required"},
		{"empty list", "[]", "OpenCode permission policy is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launch, err := startPinnedOpenCodeRuntime("spwn-pin-perm", cwd, "", tc.permission)
			require.ErrorContains(t, err, tc.refusal)
			assert.Nil(t, launch)
			row, err := db.GetOpenCodeRuntime("spwn-pin-perm")
			require.NoError(t, err)
			assert.Nil(t, row, "a refused launch records no runtime")
		})
	}
}

func TestStartOpenCodeRuntime_RefusesAnInvalidPersistedRuntimeAndKeepsIt(t *testing.T) {
	cwd := openCodeStartPin(t, "")
	existing := db.OpenCodeRuntime{
		SessionID: "spwn-pin-invalid", ServerURL: "http://127.0.0.1:43210", Cwd: cwd,
		Transport:             db.OpenCodeTransportLoopbackTCP,
		SandboxImplementation: string(sandboxpolicy.ImplementationOff),
		SandboxLaunchSpecJSON: `{"stray":true}`,
	}
	require.NoError(t, db.UpsertOpenCodeRuntime(existing))

	launch, err := startPinnedOpenCodeRuntime(existing.SessionID, cwd, "", openCodePinnedPermission)

	require.ErrorContains(t, err, "refuse invalid persisted OpenCode runtime transport: "+
		"OpenCode off runtime unexpectedly carries a tclaude sandbox launch spec")
	assert.Nil(t, launch)
	row, err := db.GetOpenCodeRuntime(existing.SessionID)
	require.NoError(t, err)
	require.NotNil(t, row, "the refused runtime is left for the operator")
	assert.Equal(t, existing.SandboxLaunchSpecJSON, row.SandboxLaunchSpecJSON)
}

func TestStartOpenCodeRuntime_RefusesToReplaceALiveUnprovenUnixRuntime(t *testing.T) {
	cwd := openCodeStartPin(t, "")
	existing := db.OpenCodeRuntime{
		SessionID: "spwn-pin-unix", ServerURL: "http://opencode.invalid", Cwd: t.TempDir(),
		PID:                 os.Getpid(), // alive, and not a server this daemon started
		Transport:           db.OpenCodeTransportUnixRelay,
		ControlSocketPath:   filepath.Join(t.TempDir(), "absent.sock"),
		ControlSocketDevice: 1, ControlSocketInode: 1,
	}
	require.NoError(t, db.UpsertOpenCodeRuntime(existing))

	launch, err := startPinnedOpenCodeRuntime(existing.SessionID, cwd, "", openCodePinnedPermission)

	require.ErrorContains(t, err,
		"refusing to replace live OpenCode Unix runtime without socket ownership proof")
	assert.Nil(t, launch)
	row, err := db.GetOpenCodeRuntime(existing.SessionID)
	require.NoError(t, err)
	require.NotNil(t, row, "the live runtime is not retired")
	assert.Equal(t, existing.PID, row.PID)
}

// A same-session runtime that cannot be reused is retired before the new
// server is started, even when that start then fails.
func TestStartOpenCodeRuntime_RetiresAStaleRuntimeBeforeLaunching(t *testing.T) {
	cwd := openCodeStartPin(t, "")
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "spwn-pin-stale", ServerURL: "http://127.0.0.1:43210",
		Cwd: t.TempDir(), Transport: db.OpenCodeTransportLoopbackTCP,
	}))

	launch, err := startPinnedOpenCodeRuntime("spwn-pin-stale", cwd, "", openCodePinnedPermission)

	require.ErrorContains(t, err, "find OpenCode executable")
	assert.Nil(t, launch)
	row, err := db.GetOpenCodeRuntime("spwn-pin-stale")
	require.NoError(t, err)
	assert.Nil(t, row, "the stale runtime was retired")
}

// A resume is keyed by its durable conversation ID; the label-keyed runtime
// of the same conversation is retired first, so two servers never own it.
func TestStartOpenCodeRuntime_ResumeRetiresTheLabelKeyedRuntime(t *testing.T) {
	cwd := openCodeStartPin(t, "")
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "spwn-pin-label", ConvID: "ses_pin_resume", ServerURL: "http://127.0.0.1:43210",
		Cwd: cwd, Transport: db.OpenCodeTransportLoopbackTCP,
	}))

	launch, err := startPinnedOpenCodeRuntime("spwn-pin-resumed", cwd, "ses_pin_resume", openCodePinnedPermission)

	require.ErrorContains(t, err, "find OpenCode executable")
	assert.Nil(t, launch)
	prior, err := db.GetOpenCodeRuntime("spwn-pin-label")
	require.NoError(t, err)
	assert.Nil(t, prior, "the label-keyed runtime was retired")
	resumed, err := db.GetOpenCodeRuntime("spwn-pin-resumed")
	require.NoError(t, err)
	assert.Nil(t, resumed, "the failed resume records no runtime")
}

func TestStartOpenCodeRuntime_GivesUpAfterThreePortAttempts(t *testing.T) {
	launches := filepath.Join(t.TempDir(), "launches")
	cwd := openCodeStartPin(t, "#!/bin/sh\necho launch >> '"+launches+"'\n"+
		"echo 'pinned server refusal' >&2\nexit 3\n")

	launch, err := startPinnedOpenCodeRuntime("spwn-pin-ports", cwd, "", openCodePinnedPermission)

	require.ErrorContains(t, err, "start OpenCode server after 3 port attempts: ")
	assert.Nil(t, launch)
	raw, err := os.ReadFile(launches)
	require.NoError(t, err)
	assert.Equal(t, 3, strings.Count(string(raw), "launch\n"), "one server per port attempt")
	row, err := db.GetOpenCodeRuntime("spwn-pin-ports")
	require.NoError(t, err)
	assert.Nil(t, row, "a server that never came up records no runtime")
}
