//go:build linux

package session

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/sandboxproxy"
	"golang.org/x/sys/unix"
)

// The proxy engine's bridge. The floor is the isolated posture's empty network
// namespace (see TclaudeLayerFloorPosture); nothing in that namespace has a
// route anywhere. This file is the one narrow exception: a loopback listener
// the sandbox binds for itself and the host then serves the filtering proxy on.
//
// The direction matters and is the reason this is not a port forward. The
// listening socket is created INSIDE the namespace, by a bootstrap that holds
// no capabilities, on an unprivileged ephemeral port; only the descriptor
// crosses out, over the same launch-private Unix packet socket and the same
// peer-credential + /proc-netns verification the DNS broker already uses. The
// host never binds anything into the sandbox, and the sandbox never receives a
// route out of it: what it receives is a socket whose only peer is a process
// outside the wall that enforces the authored policy on every request.
//
// Every failure in this file is a failure to release the harness gate, and a
// harness that never starts has no network at all. That is the fail-closed
// property stated once, here: there is no ordering in which the harness runs
// with the proxy absent.
const (
	tclaudeLayerProxyBootstrapCommand = "tclaude-layer-proxy-bootstrap"
	// proxyNetworkBootstrapSyncPath is the namespace-visible name of the
	// launch-private readiness socket. The host end is a directory only this
	// launch can read.
	proxyNetworkBootstrapSyncPath = "/tmp/.tclaude-proxy-sync.sock"
	// proxyNetworkBootstrapReady is the exact token the in-namespace bootstrap
	// sends alongside the listening descriptor.
	proxyNetworkBootstrapReady = "proxy-network-listener-ready"
	// proxyNetworkHarnessRelease is the exact token the host sends once the
	// proxy is serving. The harness execs only after reading it.
	proxyNetworkHarnessRelease = "proxy-network-ready"
	// proxyNetworkReadyTimeout bounds each half of the handshake. It mirrors
	// the packet posture's readiness bound: the work on either side is a bind
	// and a descriptor pass, so anything approaching this is wedged.
	proxyNetworkReadyTimeout = 5 * time.Second
	// proxyNetworkBootstrapWaitTimeout bounds how long the sandbox waits for
	// the host to finish. Exceeding it exits the bootstrap without exec'ing the
	// harness, which is the same fail-closed outcome as any other error here.
	proxyNetworkBootstrapWaitTimeout = 30 * time.Second
	// proxyNetworkDescriptorCount is the exact number of descriptors the
	// handoff carries. A handoff with any other count is refused rather than
	// truncated.
	proxyNetworkDescriptorCount = 1
	// proxyNetworkMinPort keeps the sandbox's bind unprivileged, which is what
	// lets the bootstrap run without CAP_NET_BIND_SERVICE.
	proxyNetworkMinPort = 1024
)

// preparedProxyNetworkRelay is the host half of the bridge, owned by the
// launch supervisor for exactly the sandbox's lifetime.
type preparedProxyNetworkRelay struct {
	SetupArgs    []string
	Command      []string
	Files        []*os.File
	SyncListener *net.UnixListener
	Sync         *net.UnixConn
	SyncDir      string
	Rules        sandboxpolicy.FilteredNetworkRuleSet
	Server       *sandboxproxy.Server
	// Wait carries the proxy's own exit. A receive on it while the sandbox is
	// still alive is a fail-closed teardown, exactly as a pasta exit is under
	// the packet engine.
	Wait <-chan error
}

// encodeProxyNetworkRelayPolicy renders the compiled policy the supervisor will
// enforce. It validates through the evaluator the proxy itself compiles, so a
// policy that reaches the relay is one the running engine can answer from.
func encodeProxyNetworkRelayPolicy(plan sandboxpolicy.MountPlan) (string, error) {
	if !tclaudeLayerPlanDeploysProxy(plan) || plan.FilteredNetwork == nil {
		return "", fmt.Errorf("proxy relay requires a compiled proxy network plan")
	}
	if _, err := sandboxproxy.NewEvaluatorFromRuleSet(*plan.FilteredNetwork); err != nil {
		return "", err
	}
	data, err := json.Marshal(plan.FilteredNetwork)
	if err != nil {
		return "", fmt.Errorf("encode proxy network policy: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	if len(encoded) > filteredNetworkPolicyEncodingLimit {
		return "", fmt.Errorf("proxy network policy exceeds the relay encoding limit")
	}
	return encoded, nil
}

// proxyNetworkRelayPrefix contributes the supervisor flag for a proxy-engine
// launch, and nothing at all for any other plan.
func proxyNetworkRelayPrefix(plan sandboxpolicy.MountPlan) (string, error) {
	if !tclaudeLayerPlanDeploysProxy(plan) {
		return "", nil
	}
	encoded, err := encodeProxyNetworkRelayPolicy(plan)
	if err != nil {
		return "", err
	}
	return " --proxy-network-policy " + clcommon.ShellQuoteArg(encoded), nil
}

// prepareProxyNetworkRelay builds the host half: the launch-private readiness
// socket, the sealed bootstrap image, and the bubblewrap arguments that make
// exactly those two things visible inside the namespace.
//
// Note what is NOT here, compared with the packet relay: no capability
// arguments, no nft policy, no hosts or resolver files, no pasta. The proxy
// engine's whole in-namespace footprint is one sealed executable, which the
// bootstrap unlinks before exec'ing the harness, and one bind-mounted readiness
// socket whose host end is closed and unlinked once the single handoff it
// exists for has been accepted.
func prepareProxyNetworkRelay(
	encoded string,
) (_ preparedProxyNetworkRelay, retErr error) {
	if strings.TrimSpace(encoded) == "" {
		return preparedProxyNetworkRelay{}, nil
	}
	rules, err := parseProxyNetworkRelayPolicy(encoded)
	if err != nil {
		return preparedProxyNetworkRelay{}, err
	}
	syncDir, err := os.MkdirTemp("", "tclaude-proxy-network-")
	if err != nil {
		return preparedProxyNetworkRelay{}, fmt.Errorf(
			"create proxy network readiness directory: %w", err)
	}
	if err := os.Chmod(syncDir, 0o700); err != nil {
		_ = os.RemoveAll(syncDir)
		return preparedProxyNetworkRelay{}, fmt.Errorf(
			"secure proxy network readiness directory: %w", err)
	}
	syncHostPath := filepath.Join(syncDir, "bootstrap.sock")
	syncListener, err := net.ListenUnix(
		"unixpacket",
		&net.UnixAddr{Name: syncHostPath, Net: "unixpacket"},
	)
	if err != nil {
		_ = os.RemoveAll(syncDir)
		return preparedProxyNetworkRelay{}, fmt.Errorf(
			"create proxy network readiness listener: %w", err)
	}
	var files []*os.File
	defer func() {
		if retErr == nil {
			return
		}
		_ = syncListener.Close()
		for _, file := range files {
			_ = file.Close()
		}
		_ = os.RemoveAll(syncDir)
	}()
	if err := os.Chmod(syncHostPath, 0o600); err != nil {
		return preparedProxyNetworkRelay{}, fmt.Errorf(
			"secure proxy network readiness listener: %w", err)
	}
	self, err := os.Open("/proc/self/exe")
	if err != nil {
		return preparedProxyNetworkRelay{}, fmt.Errorf(
			"open proxy network bootstrap image: %w", err)
	}
	files = append(files, self)
	return preparedProxyNetworkRelay{
		SetupArgs: []string{
			"--ro-bind", syncHostPath, proxyNetworkBootstrapSyncPath,
			"--perms", "0500",
			"--file", strconv.Itoa(filteredNetworkBootstrapBinaryFD),
			sandboxpolicy.FilteredNetworkBootstrapPath,
		},
		Command: []string{
			sandboxpolicy.FilteredNetworkBootstrapPath,
			"session",
			tclaudeLayerProxyBootstrapCommand,
			"--",
		},
		Files:        files,
		SyncListener: syncListener,
		SyncDir:      syncDir,
		Rules:        rules,
	}, nil
}

func parseProxyNetworkRelayPolicy(
	encoded string,
) (sandboxpolicy.FilteredNetworkRuleSet, error) {
	if len(encoded) > filteredNetworkPolicyEncodingLimit {
		return sandboxpolicy.FilteredNetworkRuleSet{}, fmt.Errorf(
			"proxy network policy exceeds the relay encoding limit")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return sandboxpolicy.FilteredNetworkRuleSet{}, fmt.Errorf(
			"decode proxy network policy: %w", err)
	}
	var rules sandboxpolicy.FilteredNetworkRuleSet
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rules); err != nil {
		return sandboxpolicy.FilteredNetworkRuleSet{}, fmt.Errorf(
			"parse proxy network policy: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return sandboxpolicy.FilteredNetworkRuleSet{}, err
	}
	// The evaluator is the engine's own acceptance test. Compiling it here
	// means an unenforceable policy is refused before a namespace exists,
	// rather than after the sandbox is already running.
	if _, err := sandboxproxy.NewEvaluatorFromRuleSet(rules); err != nil {
		return sandboxpolicy.FilteredNetworkRuleSet{}, err
	}
	return rules, nil
}

func (p *preparedProxyNetworkRelay) Close() {
	if p == nil {
		return
	}
	if p.Server != nil {
		_ = p.Server.Close()
	}
	if p.Sync != nil {
		_ = p.Sync.Close()
	}
	if p.SyncListener != nil {
		_ = p.SyncListener.Close()
	}
	for _, file := range p.Files {
		_ = file.Close()
	}
	if p.SyncDir != "" {
		_ = os.RemoveAll(p.SyncDir)
	}
}

// Active reports whether this launch deploys a proxy at all.
//
// It is keyed on the readiness directory rather than on the listener, because
// the listener is deliberately dropped the moment the handoff is accepted. A
// predicate meaning "a proxy is deployed" must not start answering false
// halfway through the launch it describes — a later supervision or teardown
// check consulting it would silently take the no-proxy branch.
func (p *preparedProxyNetworkRelay) Active() bool {
	return p != nil && p.SyncDir != ""
}

// waitCh exposes the proxy's exit channel. A nil channel blocks forever in a
// select, which is exactly the right behavior for a launch with no proxy: the
// supervision case can be written once, unconditionally.
func (p *preparedProxyNetworkRelay) waitCh() <-chan error {
	if p == nil {
		return nil
	}
	return p.Wait
}

// waitListenerReady completes the descriptor handoff and starts serving the
// policy on the namespace-owned listener.
//
// Everything the host learns about the socket it learns from the KERNEL, not
// from the peer: the peer's identity from SO_PEERCRED and /proc, the socket's
// type and listening state from getsockopt, and the address and port from
// getsockname. The bootstrap sends a fixed token and a descriptor; no field of
// the message is trusted, because a message is exactly the thing a compromised
// sandbox could forge.
func (p *preparedProxyNetworkRelay) waitListenerReady(namespacePID int) error {
	if !p.Active() {
		return nil
	}
	if p.SyncListener == nil {
		// The readiness channel exists for exactly one connection. A second
		// handshake would be a second, unverified claim on the same launch.
		return fmt.Errorf("proxy network readiness has already been consumed")
	}
	if err := p.SyncListener.SetDeadline(
		time.Now().Add(proxyNetworkReadyTimeout),
	); err != nil {
		return err
	}
	sync, err := p.SyncListener.AcceptUnix()
	if err != nil {
		return fmt.Errorf("accept proxy network readiness: %w", err)
	}
	p.Sync = sync
	_ = p.SyncListener.Close()
	p.SyncListener = nil
	// The socket has served its only purpose; unlink it so nothing can reach
	// the readiness channel after the one connection it exists for.
	_ = os.Remove(filepath.Join(p.SyncDir, "bootstrap.sock"))
	if err := validateFilteredNetworkSyncPeer(sync, namespacePID); err != nil {
		return err
	}
	if err := sync.SetReadDeadline(
		time.Now().Add(proxyNetworkReadyTimeout)); err != nil {
		return err
	}
	buffer := make([]byte, 128)
	oob := make([]byte, unix.CmsgSpace(proxyNetworkDescriptorCount*4))
	n, oobn, flags, _, err := sync.ReadMsgUnix(buffer, oob)
	if err != nil {
		return fmt.Errorf("wait for proxy network listener: %w", err)
	}
	if flags&(unix.MSG_CTRUNC|unix.MSG_TRUNC) != 0 {
		return fmt.Errorf("proxy network readiness was truncated")
	}
	if string(buffer[:n]) != proxyNetworkBootstrapReady {
		return fmt.Errorf("proxy network bootstrap returned invalid readiness")
	}
	file, err := receiveProxyNetworkListener(oob[:oobn])
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	listener, err := adoptProxyNetworkListener(file)
	if err != nil {
		return err
	}
	server, err := sandboxproxy.NewFromRuleSet(p.Rules, sandboxproxy.Config{})
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("build filtering proxy: %w", err)
	}
	p.Server = server
	wait := make(chan error, 1)
	go func() { wait <- server.Serve(listener) }()
	p.Wait = wait
	return sync.SetReadDeadline(time.Time{})
}

// receiveProxyNetworkListener extracts exactly one descriptor from the control
// message. Any other count closes everything received and refuses.
func receiveProxyNetworkListener(oob []byte) (*os.File, error) {
	messages, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, fmt.Errorf("parse proxy network descriptor handoff: %w", err)
	}
	var descriptors []int
	for _, message := range messages {
		rights, rightsErr := unix.ParseUnixRights(&message)
		if rightsErr != nil {
			for _, descriptor := range descriptors {
				_ = unix.Close(descriptor)
			}
			return nil, fmt.Errorf(
				"parse proxy network descriptor rights: %w", rightsErr)
		}
		descriptors = append(descriptors, rights...)
	}
	if len(descriptors) != proxyNetworkDescriptorCount {
		for _, descriptor := range descriptors {
			_ = unix.Close(descriptor)
		}
		return nil, fmt.Errorf(
			"proxy network descriptor handoff has %d descriptors (want %d)",
			len(descriptors), proxyNetworkDescriptorCount,
		)
	}
	return os.NewFile(uintptr(descriptors[0]), "proxy-network-listener"), nil
}

// adoptProxyNetworkListener proves the received descriptor is the thing the
// bridge contract says it is — a listening loopback TCP socket on an
// unprivileged port — before any traffic is served on it.
func adoptProxyNetworkListener(file *os.File) (net.Listener, error) {
	if err := requireProxyNetworkListeningSocket(file); err != nil {
		return nil, err
	}
	adopted, err := net.FileListener(file)
	if err != nil {
		return nil, fmt.Errorf("adopt proxy network listener: %w", err)
	}
	tcp, ok := adopted.(*net.TCPListener)
	if !ok {
		_ = adopted.Close()
		return nil, fmt.Errorf("proxy network descriptor is not a TCP listener")
	}
	address, ok := tcp.Addr().(*net.TCPAddr)
	if !ok {
		_ = tcp.Close()
		return nil, fmt.Errorf("proxy network listener has no TCP address")
	}
	addr, ok := netip.AddrFromSlice(address.IP)
	if !ok || !addr.Unmap().IsLoopback() {
		_ = tcp.Close()
		return nil, fmt.Errorf(
			"proxy network listener is bound to %s rather than sandbox loopback",
			address.IP)
	}
	if address.Port < proxyNetworkMinPort {
		_ = tcp.Close()
		return nil, fmt.Errorf(
			"proxy network listener is bound to privileged port %d", address.Port)
	}
	return tcp, nil
}

// requireProxyNetworkListeningSocket asks the kernel what the descriptor is.
// A regular file, a connected socket, or a datagram socket would each fail
// differently and later; refusing them here keeps the failure attributable.
func requireProxyNetworkListeningSocket(file *os.File) error {
	raw, err := file.SyscallConn()
	if err != nil {
		return fmt.Errorf("inspect proxy network descriptor: %w", err)
	}
	var (
		domain    int
		soType    int
		listening int
		optErr    error
	)
	if err := raw.Control(func(fd uintptr) {
		domain, optErr = unix.GetsockoptInt(
			int(fd), unix.SOL_SOCKET, unix.SO_DOMAIN)
		if optErr != nil {
			return
		}
		soType, optErr = unix.GetsockoptInt(
			int(fd), unix.SOL_SOCKET, unix.SO_TYPE)
		if optErr != nil {
			return
		}
		listening, optErr = unix.GetsockoptInt(
			int(fd), unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
	}); err != nil {
		return fmt.Errorf("inspect proxy network socket: %w", err)
	}
	if optErr != nil {
		return fmt.Errorf("inspect proxy network socket options: %w", optErr)
	}
	if domain != unix.AF_INET && domain != unix.AF_INET6 {
		return fmt.Errorf("proxy network descriptor is not an IP socket")
	}
	if soType != unix.SOCK_STREAM {
		return fmt.Errorf("proxy network descriptor is not a stream socket")
	}
	if listening != 1 {
		return fmt.Errorf("proxy network descriptor is not listening")
	}
	return nil
}

// releaseHarness opens the gate. It runs only after the proxy is serving, so
// the harness cannot observe a namespace whose only listener answers nothing.
func (p *preparedProxyNetworkRelay) releaseHarness() error {
	if p == nil || p.Sync == nil {
		return nil
	}
	if p.Server == nil {
		return fmt.Errorf("proxy network harness gate opened before the proxy served")
	}
	if _, err := p.Sync.Write([]byte(proxyNetworkHarnessRelease)); err != nil {
		return fmt.Errorf("release proxy network harness gate: %w", err)
	}
	return nil
}

func tclaudeLayerProxyBootstrapCmd() *cobra.Command {
	return &cobra.Command{
		Use:    tclaudeLayerProxyBootstrapCommand + " -- <command> [args...]",
		Short:  "Bind the sandbox proxy endpoint and enter the harness (internal)",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runTclaudeLayerProxyBootstrap(args)
		},
	}
}

// runTclaudeLayerProxyBootstrap is the in-namespace half. It holds no
// capabilities, binds one unprivileged loopback port, hands the descriptor to
// the supervisor, waits for the proxy to be serving, and only then becomes the
// harness.
//
// It returns rather than execs on every error path. A bootstrap that returns
// has not started the harness, and the sandbox it was about to enter has no
// route anywhere — the failure is closed by construction rather than by a
// cleanup step that could be forgotten.
func runTclaudeLayerProxyBootstrap(command []string) error {
	if len(command) == 0 {
		return fmt.Errorf("proxy network bootstrap contract is invalid")
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{
		IP:   net.IPv4(127, 0, 0, 1),
		Port: 0,
	})
	if err != nil {
		return fmt.Errorf("bind sandbox proxy endpoint: %w", err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return fmt.Errorf("sandbox proxy endpoint has no TCP address")
	}
	port := address.Port
	// File duplicates the descriptor, so the socket outlives the listener
	// object. Closing the listener leaves nothing inside the namespace holding
	// the endpoint open except the descriptor about to cross out.
	file, err := listener.File()
	closeListener := listener.Close()
	if err != nil {
		return fmt.Errorf("export sandbox proxy endpoint: %w", err)
	}
	defer func() { _ = file.Close() }()
	if closeListener != nil {
		return fmt.Errorf("release sandbox proxy endpoint object: %w", closeListener)
	}
	if port < proxyNetworkMinPort {
		return fmt.Errorf("sandbox proxy endpoint bound privileged port %d", port)
	}
	sync, err := net.DialUnix(
		"unixpacket",
		nil,
		&net.UnixAddr{Name: proxyNetworkBootstrapSyncPath, Net: "unixpacket"},
	)
	if err != nil {
		return fmt.Errorf("connect proxy network readiness channel: %w", err)
	}
	defer func() { _ = sync.Close() }()
	if _, _, err := sync.WriteMsgUnix(
		[]byte(proxyNetworkBootstrapReady),
		unix.UnixRights(int(file.Fd())),
		nil,
	); err != nil {
		return fmt.Errorf("hand out sandbox proxy endpoint: %w", err)
	}
	if err := sync.SetReadDeadline(
		time.Now().Add(proxyNetworkBootstrapWaitTimeout)); err != nil {
		return err
	}
	buffer := make([]byte, 64)
	n, err := sync.Read(buffer)
	if err != nil {
		return fmt.Errorf("wait for filtering proxy readiness: %w", err)
	}
	if string(buffer[:n]) != proxyNetworkHarnessRelease {
		return fmt.Errorf("filtering proxy returned invalid readiness")
	}
	if err := sync.Close(); err != nil {
		return fmt.Errorf("close proxy network readiness authority: %w", err)
	}
	_ = os.Remove(sandboxpolicy.FilteredNetworkBootstrapPath)
	executable := command[0]
	if !filepath.IsAbs(executable) {
		executable, err = exec.LookPath(executable)
		if err != nil {
			return fmt.Errorf("resolve proxy network harness executable: %w", err)
		}
	}
	return unix.Exec(executable, command, proxyNetworkSandboxEnv(os.Environ(), port))
}

// proxyNetworkProxyVariables are the variables the launcher owns on a
// proxy-engine launch. Every one of them is REPLACED rather than merged: an
// inherited value would point the harness at a destination tclaude does not
// supervise, and an inherited NO_PROXY would carve destinations back out of the
// only route that exists.
var proxyNetworkProxyVariables = []string{
	"HTTP_PROXY", "http_proxy",
	"HTTPS_PROXY", "https_proxy",
	"ALL_PROXY", "all_proxy",
	"NO_PROXY", "no_proxy",
}

// proxyNetworkSandboxEnv injects the sandbox's proxy discovery.
//
// ALL_PROXY uses socks5h rather than socks5, and the h is the whole point: it
// keeps name resolution at the proxy, where the authored host and domain rows
// are evaluated. A client that resolved names itself would have nothing to
// resolve with — the namespace has no resolver — and would ask the proxy for a
// literal, which the authored name rows do not cover.
//
// NO_PROXY and no_proxy are set to the empty string rather than removed. Empty
// is the value that exempts nothing; absent would let a harness fall back to
// its own default exemption list, which commonly includes localhost and private
// space.
func proxyNetworkSandboxEnv(environ []string, port int) []string {
	owned := make(map[string]struct{}, len(proxyNetworkProxyVariables))
	for _, name := range proxyNetworkProxyVariables {
		owned[name] = struct{}{}
	}
	out := make([]string, 0, len(environ)+len(proxyNetworkProxyVariables))
	for _, pair := range environ {
		name, _, ok := strings.Cut(pair, "=")
		if ok {
			if _, mine := owned[name]; mine {
				continue
			}
		}
		out = append(out, pair)
	}
	endpoint := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	http := "http://" + endpoint
	socks := "socks5h://" + endpoint
	return append(out,
		"HTTP_PROXY="+http,
		"http_proxy="+http,
		"HTTPS_PROXY="+http,
		"https_proxy="+http,
		"ALL_PROXY="+socks,
		"all_proxy="+socks,
		"NO_PROXY=",
		"no_proxy=",
	)
}
