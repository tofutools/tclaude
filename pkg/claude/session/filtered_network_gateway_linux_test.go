//go:build linux

package session

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
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

func TestFilteredNetworkPastaArgsGiveSyntheticIPv6MappingARoute(t *testing.T) {
	assert.Equal(t, []string{
		"--foreground",
		"--quiet",
		"--config-net",
		"--gateway", sandboxpolicy.FilteredNetworkGatewayIPv6,
		"--no-map-gw",
		"--map-guest-addr", "none",
		"--map-host-loopback", sandboxpolicy.FilteredNetworkLoopbackIPv4,
		"--map-host-loopback", sandboxpolicy.FilteredNetworkLoopbackIPv6,
		"--tcp-ports", "none",
		"--udp-ports", "none",
		"--tcp-ns", "none",
		"--udp-ns", "none",
		"--no-splice",
		"--pid", "/tmp/pasta.pid",
		"123",
	}, filteredNetworkPastaArgs("/tmp/pasta.pid", 123))
}

func TestFilteredNetworkPastaReadinessRetriesPartialPIDFile(t *testing.T) {
	root := t.TempDir()
	pastaPath := filepath.Join(root, "pasta")
	require.NoError(t, os.WriteFile(pastaPath, []byte(`#!/bin/sh
pidfile=
while [ "$#" -gt 0 ]; do
	case "$1" in
		--pid)
			pidfile=$2
			shift 2
			;;
		*)
			shift
			;;
	esac
done
printf 'partial' > "$pidfile"
sleep 0.1
printf '%s\n' "$$" > "$pidfile"
while :; do sleep 1; done
`), 0o700))

	relay := &preparedFilteredNetworkRelay{
		PastaPath:    pastaPath,
		PastaPIDFile: filepath.Join(root, "pasta.pid"),
	}
	cmd, waitCh, err := relay.startPasta(os.Getpid())
	require.NoError(t, err)
	require.NotNil(t, cmd)
	require.NoError(t, cmd.Process.Signal(syscall.Signal(0)))
	require.NoError(t, cmd.Process.Kill())
	require.Error(t, <-waitCh)
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
