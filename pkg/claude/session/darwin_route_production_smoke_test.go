//go:build darwin

package session

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TestDarwinRouteCapabilityExactHeadSmoke is the authoritative route-capable
// launch-contract cell. It deliberately fails when the workflow forgets to
// enable the cell: a skipped macOS route test is not evidence.
func TestDarwinRouteCapabilityExactHeadSmoke(t *testing.T) {
	if os.Getenv("TCLAUDE_DARWIN_ROUTE_CAPABILITY_SMOKE") != "1" {
		t.Fatal("TCL-951 Darwin route smoke is inactive; set TCLAUDE_DARWIN_ROUTE_CAPABILITY_SMOKE=1")
	}
	headBytes, err := exec.Command("git", "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	head := strings.TrimSpace(string(headBytes))
	require.Len(t, head, 40)
	t.Logf("TCL-951 exact checked-out head: %s", head)

	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	reservation, err := ReserveDarwinRouteSlots()
	require.NoError(t, err)
	defer func() { require.NoError(t, reservation.Release()) }()
	slots := reservation.Slots()
	require.NotEmpty(t, slots)
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.NetworkAccess = sandboxpolicy.NetworkAccessNone
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
		HarnessName:            harness.DefaultName,
		Cwd:                    cwd,
		Snapshot:               &snapshot,
		DarwinRouteSlots:       slots,
		DarwinRouteReservation: reservation,
	})
	require.NoError(t, err)
	encoded, err := json.Marshal(spec)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "DarwinRouteReservation")
	command, err := WrapTclaudeLayerSpec("/usr/bin/true", spec, "printf route-capable")
	require.NoError(t, err)
	require.Contains(t, command, DarwinRouteSlotsEnv+"=")
	for _, slot := range slots {
		require.Contains(t, command, fmt.Sprintf("localhost:%d", slot))
	}
	neighbor := slots[len(slots)-1] + 1
	require.NotContains(t, command, fmt.Sprintf("localhost:%d", neighbor))
	t.Logf("TCL-951 route launch contract: POSITIVE slots=%v endpoint=opaque adapter path", slots)
	t.Log("TCL-951 disclosure: Partial; Seatbelt localhost tokens retain the documented same-port local limitation")
}
