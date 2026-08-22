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
)

func TestFilteredNetworkHelperEnvExcludesAmbientInjectionVariables(t *testing.T) {
	assert.Equal(t, []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C",
		"LC_ALL=C",
	}, filteredNetworkHelperEnv())
}

func TestFilteredNetworkNsenterArgsJoinOwnerUsernsFirst(t *testing.T) {
	// fd 3 is the netns's OWNING user namespace, fd 4 the netns itself, matching
	// the ExtraFiles order installBasePolicy passes. --preserve-credentials is
	// mandatory because bubblewrap sets setgroups=deny on the sandbox userns.
	assert.Equal(t, []string{
		"--preserve-credentials",
		"--user=/proc/self/fd/3",
		"--net=/proc/self/fd/4",
		"--",
		"/usr/sbin/nft", "-f", "-",
	}, filteredNetworkNsenterArgs("/usr/sbin/nft"))
}

func TestFilteredNetworkInstallBasePolicyValidatesInputs(t *testing.T) {
	// A nil relay is a no-op (non-filtered launch).
	var nilRelay *preparedFilteredNetworkRelay
	require.NoError(t, nilRelay.installBasePolicy(123))
	// An active relay with no rendered ruleset must fail closed rather than come
	// up unfiltered.
	require.ErrorContains(t,
		(&preparedFilteredNetworkRelay{}).installBasePolicy(123), "no rendered policy")
	// A rendered policy with no helper paths must fail loudly rather than exec a
	// bare command name.
	relay := &preparedFilteredNetworkRelay{Policy: "flush ruleset\n"}
	require.ErrorContains(t, relay.installBasePolicy(123), "helper paths")
	// With no pinned namespace fd, an invalid namespace pid is rejected before
	// any namespace work.
	relay = &preparedFilteredNetworkRelay{
		Policy: "flush ruleset\n", NsenterPath: "/usr/bin/nsenter", NFTPath: "/usr/sbin/nft",
	}
	require.ErrorContains(t, relay.installBasePolicy(0), "namespace pid")
}

func TestFilteredNetworkPastaArgsJoinNetworkNamespaceOwner(t *testing.T) {
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
		"--userns", "/proc/self/fd/3",
		"--netns", "/proc/self/fd/4",
	}, filteredNetworkPastaArgs("/tmp/pasta.pid"))
}

func TestFilteredNetworkResolvMountMaterializesRuntimeSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	configRoot := filepath.Join(root, "etc")
	runtimeRoot := filepath.Join(root, "run")
	target := filepath.Join(runtimeRoot, "systemd", "resolve", "stub-resolv.conf")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o700))
	require.NoError(t, os.MkdirAll(configRoot, 0o700))
	require.NoError(t, os.WriteFile(target, []byte("host resolver"), 0o600))
	resolv := filepath.Join(configRoot, "resolv.conf")
	require.NoError(t, os.Symlink(
		filepath.Join("..", "run", "systemd", "resolve", "stub-resolv.conf"),
		resolv))

	destination, dirs, err := filteredNetworkResolvMount(resolv, runtimeRoot)
	require.NoError(t, err)
	assert.Equal(t, target, destination)
	assert.Equal(t, []string{
		runtimeRoot,
		filepath.Join(runtimeRoot, "systemd"),
		filepath.Join(runtimeRoot, "systemd", "resolve"),
	}, dirs)
	assert.Equal(t, []string{
		"--dir", filepath.Join(runtimeRoot, "systemd"),
		"--dir", filepath.Join(runtimeRoot, "systemd", "resolve"),
	}, appendFilteredNetworkResolvDirs(nil, dirs))

	outside := filepath.Join(root, "home", "resolver")
	require.NoError(t, os.MkdirAll(filepath.Dir(outside), 0o700))
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))
	require.NoError(t, os.Remove(resolv))
	require.NoError(t, os.Symlink(outside, resolv))
	_, _, err = filteredNetworkResolvMount(resolv, runtimeRoot)
	require.ErrorContains(t, err, "outside supported")
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
