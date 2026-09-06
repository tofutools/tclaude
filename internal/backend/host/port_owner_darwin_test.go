//go:build darwin

package host

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDarwinLoopbackOwnershipIncludesLauncherDescendant(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	process, err := StartProcess(ProcessSpec{
		Executable: os.Args[0], Args: []string{"-test.run=TestDarwinLauncherHelper", "--", strconv.Itoa(port)},
		Env: []string{"TCLAUDE_DARWIN_LAUNCHER_HELPER=1"},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _, _ = process.Stop(ctx, true)
	})
	require.Eventually(t, func() bool {
		owned, ownErr := process.OwnsLoopbackPort(port)
		return ownErr == nil && owned
	}, 2*time.Second, 20*time.Millisecond)
}

func TestDarwinLauncherHelper(t *testing.T) {
	if os.Getenv("TCLAUDE_DARWIN_LAUNCHER_HELPER") != "1" {
		return
	}
	args := argumentsAfterDoubleDash(os.Args)
	cmd := exec.Command(os.Args[0], "-test.run=TestDarwinListenerHelper", "--", args[0])
	cmd.Env = MergeEnvironment(os.Environ(), []string{"TCLAUDE_DARWIN_LISTENER_HELPER=1"})
	require.NoError(t, cmd.Start())
	require.NoError(t, cmd.Wait())
}

func TestDarwinListenerHelper(t *testing.T) {
	if os.Getenv("TCLAUDE_DARWIN_LISTENER_HELPER") != "1" {
		return
	}
	args := argumentsAfterDoubleDash(os.Args)
	listener, err := net.Listen("tcp", "127.0.0.1:"+args[0])
	require.NoError(t, err)
	defer listener.Close()
	waitForTestProcessStop()
}

func argumentsAfterDoubleDash(args []string) []string {
	for index, value := range args {
		if value == "--" && index+1 < len(args) {
			return args[index+1:]
		}
	}
	return nil
}
