//go:build linux || darwin

package host

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUnixControlRequiresRetainedListenerAndProcessBeforeSending(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "uc-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	path := filepath.Join(root, "socket")
	process, err := StartProcess(ProcessSpec{Executable: os.Args[0], Args: []string{"-test.run=^TestUnixControlListenerFixture$"}, Env: []string{"TCLAUDE_CONTROL_FIXTURE=" + path}})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, exited, err := process.Stop(ctx, true)
		require.NoError(t, err)
		require.True(t, exited)
	})
	var identity UnixControlIdentity
	require.Eventually(t, func() bool {
		identity, err = InspectUnixControl(path)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	recovered, err := RecoverProcess(process.Identity())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connection, err := recovered.DialUnixControl(ctx, identity)
	require.NoError(t, err)
	require.NoError(t, connection.SetDeadline(time.Now().Add(time.Second)))
	_, err = connection.Write([]byte("exact request"))
	require.NoError(t, err)
	body := make([]byte, len("exact request"))
	_, err = io.ReadFull(connection, body)
	require.NoError(t, err)
	require.Equal(t, "exact request", string(body))
	require.NoError(t, connection.Close())
	// A replacement listener must fail even while the old process is alive.
	require.NoError(t, os.Rename(path, path+".old"))
	replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	require.NoError(t, err)
	defer replacement.Close()
	require.NoError(t, os.Chmod(path, 0600))
	_, err = recovered.DialUnixControl(ctx, identity)
	require.ErrorContains(t, err, "identity changed")
	// Even a caller substituting the replacement's file identity cannot
	// make a different process receive the native credential.
	changed, err := InspectUnixControl(path)
	require.NoError(t, err)
	_, err = recovered.DialUnixControl(ctx, changed)
	require.ErrorContains(t, err, "retained process")
	peer, err := replacement.AcceptUnix()
	require.NoError(t, err)
	defer peer.Close()
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = peer.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)
}

func TestUnixControlListenerFixture(t *testing.T) {
	path := os.Getenv("TCLAUDE_CONTROL_FIXTURE")
	if path == "" {
		return
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	require.NoError(t, err)
	listener.SetUnlinkOnClose(false)
	require.NoError(t, os.Chmod(path, 0600))
	for {
		connection, err := listener.Accept()
		require.NoError(t, err)
		go func() { _, _ = io.Copy(connection, connection); _ = connection.Close() }()
	}
}
