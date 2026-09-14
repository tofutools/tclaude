//go:build linux

package session

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Invoked by the existing executing packet smoke so CI exercises an actual
// running supervisor, the private DB mailbox, nft replacement and TCP/UDP.
func runNetworkSyncSmoke(t *testing.T, bwrap, helper, workspace, home string, oldPort, newPort int) {
	t.Helper()
	db.ResetForTest()
	t.Cleanup(db.Close)
	rules := sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeList, Allow: []sandboxpolicy.NetworkAllowEntry{{Loopback: true, Ports: []int{oldPort}}}}
	profile := &db.SandboxProfile{Name: "live-network", Network: &rules}
	id, err := db.CreateSandboxProfile(profile)
	require.NoError(t, err)
	profile.ID = id
	snapshot, err := db.ResolveEffectiveSandboxSnapshot(0, profile.Name)
	require.NoError(t, err)
	spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{HarnessName: harness.DefaultName, Cwd: workspace, Snapshot: &snapshot, StateRoot: filepath.Join(home, ".claude")})
	require.NoError(t, err)
	require.NoError(t, PrepareNetworkSyncLaunch(&spec, &snapshot, "live-smoke"))
	command, err := WrapTclaudeLayerSpec(bwrap, spec, clcommon.ShellQuoteArg(helper)+" -test.run=^TestNetworkSyncGatewayHelper$")
	require.NoError(t, err)
	ready := filepath.Join(workspace, "sync-ready")
	applied := filepath.Join(workspace, "sync-applied")
	logPath := filepath.Join(workspace, "sync.log")
	log, err := os.Create(logPath)
	require.NoError(t, err)
	defer func() { _ = log.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Env = append(os.Environ(), "TCLAUDE_SYNC_SMOKE_READY="+ready, "TCLAUDE_SYNC_SMOKE_APPLIED="+applied, fmt.Sprintf("TCLAUDE_SYNC_SMOKE_OLD=%d", oldPort), fmt.Sprintf("TCLAUDE_SYNC_SMOKE_NEW=%d", newPort))
	cmd.Stdout = log
	cmd.Stderr = log
	require.NoError(t, cmd.Start())
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	waitForFilteredSmokeReady(t, ready, wait, logPath)
	require.Eventually(t, func() bool { _, err := db.ReadNetworkSyncLaunch(spec.Contract.NetworkSyncID); return err == nil }, 10*time.Second, 20*time.Millisecond, "supervisor registration")
	profile.Network = &sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeList, Allow: []sandboxpolicy.NetworkAllowEntry{{Loopback: true, Ports: []int{newPort}}}}
	require.NoError(t, db.UpdateSandboxProfile(profile))
	queued, err := db.QueueProfileNetworkSync(id, true)
	require.NoError(t, err)
	require.Len(t, queued, 1)
	var status *db.NetworkSyncLaunch
	require.Eventually(t, func() bool {
		status, err = db.ReadNetworkSyncLaunch(spec.Contract.NetworkSyncID)
		return err == nil && status.Status == "applied" && status.Acknowledged == queued[0].Revision
	}, 15*time.Second, 50*time.Millisecond, "status=%+v", status)
	require.NoError(t, os.WriteFile(applied, []byte("applied"), 0o600))
	runErr := <-wait
	output, _ := os.ReadFile(logPath)
	require.NoErrorf(t, runErr, "live reload smoke: %s", output)
}

func TestNetworkSyncGatewayHelper(t *testing.T) {
	ready := os.Getenv("TCLAUDE_SYNC_SMOKE_READY")
	if ready == "" {
		t.Skip("sandbox-only helper")
	}
	oldPort := requireFilteredSmokePort(t, "TCLAUDE_SYNC_SMOKE_OLD")
	newPort := requireFilteredSmokePort(t, "TCLAUDE_SYNC_SMOKE_NEW")
	oldAddr := net.JoinHostPort(sandboxpolicy.FilteredNetworkLoopbackIPv4, fmt.Sprint(oldPort))
	newAddr := net.JoinHostPort(sandboxpolicy.FilteredNetworkLoopbackIPv4, fmt.Sprint(newPort))
	connection, err := net.DialTimeout("tcp4", oldAddr, filteredGatewayConnectionTimeout)
	require.NoError(t, err)
	defer func() { _ = connection.Close() }()
	filteredSmokeTCPEchoOnConnection(t, connection)
	require.NoError(t, os.WriteFile(ready, []byte("ready"), 0o600))
	require.Eventually(t, func() bool { _, err := os.Stat(os.Getenv("TCLAUDE_SYNC_SMOKE_APPLIED")); return err == nil }, 20*time.Second, 50*time.Millisecond)
	filteredSmokeTCPEchoDeniedOnConnection(t, connection)
	filteredSmokeTCPRoundTrip(t, "tcp4", newAddr)
	filteredSmokeUDPRoundTrip(t, "udp4", newAddr)
}
