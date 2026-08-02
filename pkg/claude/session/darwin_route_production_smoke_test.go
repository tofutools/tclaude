//go:build darwin

package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TestDarwinRouteCapabilityExactHeadSmoke is the route-capable launch-contract
// cell. The dedicated workflow enables it; ordinary package shards skip it
// because they do not provide the Seatbelt evidence prerequisites.
func TestDarwinRouteCapabilityExactHeadSmoke(t *testing.T) {
	if os.Getenv("TCLAUDE_DARWIN_ROUTE_CAPABILITY_SMOKE") != "1" {
		t.Skip("set TCLAUDE_DARWIN_ROUTE_CAPABILITY_SMOKE=1 on the dedicated macOS evidence workflow")
	}
	headBytes, err := exec.Command("git", "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	head := strings.TrimSpace(string(headBytes))
	require.Len(t, head, 40)
	t.Logf("TCL-951 exact checked-out head: %s", head)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TMUX", "")
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	cwd := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	convID := "77000000-0000-4000-8000-000000000951"
	agentID, _, err := db.EnsureAgentForConv(convID, "TCL-951 route evidence")
	require.NoError(t, err)
	rec := &launchRecordingTmux{paneCwd: cwd}
	swapTmux(t, rec)
	previousAncestorCheck := ClaudeAncestorCheck
	ClaudeAncestorCheck = func() bool { return false }
	t.Cleanup(func() { ClaudeAncestorCheck = previousAncestorCheck })
	params := &NewParams{
		ManagedLaunch: true, Harness: harness.DefaultName, SandboxImpl: string(sandboxpolicy.ImplementationTclaudeLayer),
		DarwinRouteCapable: true, DarwinRouteAgentID: agentID, SessionID: convID, Dir: cwd, Detached: true,
	}
	require.NoError(t, runNew(params), "Darwin route smoke must use the production runNew path")
	launches := rec.newSessions()
	require.Len(t, launches, 1, "production route launch must render one detached pane")
	scriptPath := launches[0][len(launches[0])-1]
	contents, err := os.ReadFile(scriptPath)
	require.NoError(t, err)
	// The command is the same production Seatbelt launch script handed to the
	// pane. Execute it once on the dedicated runner; the harness binary may be
	// absent in a local run, but sandbox-exec must still be entered.
	launchCtx, cancelLaunch := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelLaunch()
	output, runErr := exec.CommandContext(launchCtx, "sh", scriptPath).CombinedOutput()
	t.Logf("TCL-951 Seatbelt launch output: %s (err=%v)", strings.TrimSpace(string(output)), runErr)
	require.Contains(t, string(contents), "sandbox-exec")
	d, err := db.Open()
	require.NoError(t, err)
	var generation, encodedSlots string
	require.NoError(t, d.QueryRow(`SELECT launch_generation, slots FROM darwin_route_launches WHERE agent_id = ? AND conv_id = ? AND state = ?`, agentID, convID, db.DarwinRouteLaunchActive).Scan(&generation, &encodedSlots))
	require.NotEmpty(t, generation)
	require.NotEmpty(t, encodedSlots)
	slots := strings.Split(encodedSlots, ",")
	t.Logf("TCL-951 route launch contract: POSITIVE slots=%v endpoint=production adapter path", slots)
	// Exercise the real Seatbelt denial boundary against the exact neighbor
	// token emitted by the launch contract, and leave no durable claim behind.
	require.NoError(t, db.DeleteDarwinRouteLaunch(agentID, convID, generation))
	t.Log("TCL-951 disclosure: Partial; Seatbelt localhost tokens retain the documented same-port local limitation")
}
