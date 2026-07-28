//go:build linux

package session

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"golang.org/x/sys/unix"
)

func TestFilteredNetworkHelperEnvExcludesAmbientInjectionVariables(t *testing.T) {
	assert.Equal(t, []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C",
		"LC_ALL=C",
	}, filteredNetworkHelperEnv())
}

func TestFilteredNetworkNFTCommandCarriesOnlyBootstrapCapability(t *testing.T) {
	cmd := filteredNetworkNFTCommand("/usr/sbin/nft")
	assert.Equal(t, []string{
		"/usr/sbin/nft",
		"-f",
		sandboxpolicy.FilteredNetworkNFTPolicyPath,
	}, cmd.Args)
	assert.Equal(t, []uintptr{unix.CAP_NET_ADMIN}, cmd.SysProcAttr.AmbientCaps)
	assert.Equal(t, filteredNetworkHelperEnv(), cmd.Env)
}

func TestFilteredNetworkReadinessAuthenticatesSandboxNetworkNamespace(t *testing.T) {
	socketPath := filepath.Join(agentipctest.ShortSocketDir(t), "ready.sock")
	listener, err := net.ListenUnix(
		"unixpacket",
		&net.UnixAddr{Name: socketPath, Net: "unixpacket"},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	clientCh := make(chan *net.UnixConn, 1)
	go func() {
		client, dialErr := net.DialUnix(
			"unixpacket",
			nil,
			&net.UnixAddr{Name: socketPath, Net: "unixpacket"},
		)
		if dialErr == nil {
			clientCh <- client
		}
	}()
	server, err := listener.AcceptUnix()
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	client := <-clientCh
	t.Cleanup(func() { _ = client.Close() })

	require.NoError(t, validateFilteredNetworkSyncPeer(server, os.Getpid()))
	require.Error(t, validateFilteredNetworkSyncPeer(server, 1<<30))
}
