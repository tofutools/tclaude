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
	for _, trap := range []string{"trap '' INT", "trap 'exit 0' INT"} {
		t.Run(trap, func(t *testing.T) { testStopOpenCodeWorkloadGroup(t, trap) })
	}
}

func testStopOpenCodeWorkloadGroup(t *testing.T, trap string) {
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	defer reader.Close()
	defer writer.Close()
	// Exercise both a stuck bridge and a prompt exit that reparents the workload.
	// The inherited descriptor proves the background workload exits too.
	cmd := exec.Command(clcommon.BootstrapShellPath(), "-c", trap+"; sleep 60 & printf ready >&3; wait")
	configureOpenCodeProcessGroup(cmd)
	cmd.ExtraFiles = []*os.File{writer}
	require.NoError(t, cmd.Start())
	process := &openCodeProcess{cmd: cmd, done: make(chan error, 1)}
	go func() { err := cmd.Wait(); process.killWorkloadGroup(); process.finish(err) }()
	t.Cleanup(func() {
		select {
		case <-process.done:
			return
		default:
			killOpenCodeProcessGroup(cmd)
			_ = cmd.Process.Kill()
		}
	})
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
