package agentd

import (
	"os"
	"os/exec"
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
	return testutil.CanonicalTempDir(t)
}

func startPinnedOpenCodeRuntime(sessionID, cwd, resumeID, permissionJSON string) (*openCodeLaunch, error) {
	return startOpenCodeRuntime(sessionID, cwd, "pin title", resumeID, permissionJSON,
		string(sandboxpolicy.ImplementationOff), nil, "")
}

// recordOpenCodeCgroupRemovals swaps the cgroup removal for a recorder.
func recordOpenCodeCgroupRemovals(t *testing.T) *[]string {
	t.Helper()
	previous := removeOpenCodeResourceCgroup
	t.Cleanup(func() { removeOpenCodeResourceCgroup = previous })
	var removed []string
	removeOpenCodeResourceCgroup = func(dir string) error {
		removed = append(removed, dir)
		return nil
	}
	return &removed
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
	live := openCodePinnedLiveProcess(t) // before openCodeStartPin narrows PATH
	cwd := openCodeStartPin(t, "")
	existing := db.OpenCodeRuntime{
		SessionID: "spwn-pin-unix", ServerURL: "http://opencode.invalid", Cwd: t.TempDir(),
		PID:                 live, // alive, and not a server this daemon started
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

// openCodePinnedLiveProcess is a live child the test owns, so a regression
// that retires the runtime kills it rather than the test binary.
func openCodePinnedLiveProcess(t *testing.T) int {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	require.NoError(t, err)
	cmd := exec.Command(sleep, "60")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// A same-session replacement that reuses the retired server's resource cgroup
// keeps it while it retires the old server, and removes it once when the new
// launch then fails, so a failed replacement never orphans the cgroup.
func TestStartOpenCodeRuntime_AFailedReplacementRemovesTheReusedCgroupOnce(t *testing.T) {
	cwd := openCodeStartPin(t, "")
	removed := recordOpenCodeCgroupRemovals(t)
	const cgroup = "/sys/fs/cgroup/system.slice/tclaude-tmux.service/tclaude-pin"
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "spwn-pin-cgroup", ServerURL: "http://127.0.0.1:43210",
		Cwd: t.TempDir(), Transport: db.OpenCodeTransportLoopbackTCP,
		ResourceCgroupDir: cgroup,
	}))

	launch, err := startOpenCodeRuntime("spwn-pin-cgroup", cwd, "pin title", "",
		openCodePinnedPermission, string(sandboxpolicy.ImplementationOff), nil, cgroup)

	require.ErrorContains(t, err, "find OpenCode executable")
	assert.Nil(t, launch)
	assert.Equal(t, []string{cgroup}, *removed,
		"the reused cgroup is kept through retirement and removed once on failure")
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
	assert.Contains(t, err.Error(), "pinned server refusal", "the error carries the server's stderr")
	assert.Nil(t, launch)
	raw, err := os.ReadFile(launches)
	require.NoError(t, err)
	assert.Equal(t, 3, strings.Count(string(raw), "launch\n"), "one server per port attempt")
	row, err := db.GetOpenCodeRuntime("spwn-pin-ports")
	require.NoError(t, err)
	assert.Nil(t, row, "a server that never came up records no runtime")
}
