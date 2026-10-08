package host

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/hostmetrics"
)

func TestHostStatusRendering(t *testing.T) {
	s := statusReadout{Snapshot: hostmetrics.Snapshot{OS: "darwin", Arch: "arm64", CPU: hostmetrics.CPU{LogicalCores: 8}, RAM: &hostmetrics.Memory{TotalBytes: 16 << 30, AvailableBytes: 1 << 30, AvailableEstimated: true}, Disks: []hostmetrics.Disk{{Path: hostmetrics.Path{Kind: "work", Path: "/work/new"}, MeasuredPath: "/work", TotalBytes: 100 << 30, AvailableBytes: 1 << 30}}, Tclaude: &hostmetrics.AgentLoad{LiveAgents: 2, LiveSessions: 3}}, Status: "current", Warnings: []hostmetrics.Warning{{Code: "low_disk", Resource: "/work/new", Message: "disk is low"}}}
	raw, err := json.Marshal(s)
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, renderStatus(raw, &out))
	for _, part := range []string{"darwin/arm64", "8 logical cores", "load unavailable", "1.0 GiB available / 16.0 GiB total (estimated available)", "via /work", "2 live agents; 3 live sessions", "Warning [low_disk]"} {
		require.Contains(t, out.String(), part)
	}
	out.Reset()
	require.NoError(t, renderStatus(json.RawMessage(`{"status":"warming","cpu":{},"disks":[]}`), &out))
	require.Contains(t, out.String(), "RAM: unavailable")
	require.Contains(t, out.String(), "tclaude: load unavailable")
	cmd, _, err := Cmd().Find([]string{"status"})
	require.NoError(t, err)
	require.NotNil(t, cmd.Flags().Lookup("json"))
	require.NotNil(t, cmd.Flags().Lookup("no-cache"))
}
