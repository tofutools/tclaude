//go:build linux || darwin

package agentd

import (
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestStopOpenCodeProcessTerminatesWorkloadGroup(t *testing.T) {
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	defer reader.Close()
	defer writer.Close()
	// Both the bridge-shaped shell and its workload ignore interrupt, forcing
	// the production timeout path. The inherited fd proves the workload exits.
	cmd := exec.Command(clcommon.BootstrapShellPath(), "-c", "trap '' INT; sleep 60 & printf ready >&3; wait")
	configureOpenCodeProcessGroup(cmd)
	cmd.ExtraFiles = []*os.File{writer}
	require.NoError(t, cmd.Start())
	process := &openCodeProcess{cmd: cmd, done: make(chan error, 1)}
	go func() { process.finish(cmd.Wait()) }()
	t.Cleanup(func() { killOpenCodeProcessGroup(cmd); _ = cmd.Process.Kill() })
	require.NoError(t, writer.Close())
	ready := make([]byte, 5)
	_, err = io.ReadFull(reader, ready)
	require.NoError(t, err)
	require.Equal(t, "ready", string(ready))
	eof := make(chan error, 1)
	go func() { _, err := io.ReadAll(reader); eof <- err }()
	stopOpenCodeProcess(db.OpenCodeRuntime{SessionID: "test-stop-http-workload", PID: cmd.Process.Pid}, process)
	select {
	case err := <-eof:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("workload retained its inherited descriptor after bridge stop")
	}
}
