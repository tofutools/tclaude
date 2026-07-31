//go:build linux

package session

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"golang.org/x/sys/unix"
)

const (
	tclaudeLayerFilteredBootstrapCommand = "tclaude-layer-filtered-bootstrap"
	tclaudeLayerFilteredNFTCommand       = "tclaude-layer-filtered-nft"
	filteredNetworkPolicyEncodingLimit   = 64 << 10
	filteredNetworkBootstrapBinaryFD     = 4
	filteredNetworkPolicyFD              = 5
	filteredNetworkHostsFD               = 6
	filteredNetworkResolvFD              = 7
	filteredNetworkPastaReadyTimeout     = 5 * time.Second
	filteredNetworkBootstrapSyncPath     = "/tmp/.tclaude-filtered-sync.sock"
	filteredNetworkDNSDescriptorCount    = 3
)

// The packet gateway's four sealed inputs end at filteredNetworkResolvFD, and
// the shared relay fd arithmetic in sandbox_bwrap.go is written against that
// count: the OpenCode launcher's preserved descriptors begin immediately after
// it. This is a compile-time pin, not a comment — adding a fifth sealed input
// here without raising tclaudeLayerPacketEngineDescriptors fails the build,
// rather than shipping an inherited relay that names two fds nothing installed.
var _ = [1]struct{}{}[filteredNetworkResolvFD-
	(tclaudeLayerRelayStatusFD+tclaudeLayerPacketEngineDescriptors)]

func filteredNetworkHelperEnv() []string {
	return []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C",
		"LC_ALL=C",
	}
}

func filteredNetworkBootstrapCapabilityArgs() []string {
	return []string{
		"--cap-add", "CAP_NET_ADMIN",
		"--cap-add", "CAP_NET_BIND_SERVICE",
	}
}

func filteredNetworkNFTCommand(nftPath string) *exec.Cmd {
	cmd := exec.Command(
		sandboxpolicy.FilteredNetworkBootstrapPath,
		"session",
		tclaudeLayerFilteredNFTCommand,
		"--nft", nftPath,
	)
	cmd.Env = filteredNetworkHelperEnv()
	// The parent narrows every capability set before this additive ambient
	// request. The internal child verifies the resulting exact sets before it
	// execs nft; the later harness exec follows an explicit all-set drop.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		AmbientCaps: []uintptr{unix.CAP_NET_ADMIN},
	}
	return cmd
}

type preparedFilteredNetworkRelay struct {
	SetupArgs    []string
	Command      []string
	Files        []*os.File
	SyncListener *net.UnixListener
	Sync         *net.UnixConn
	PastaPath    string
	PastaPIDFile string
	Rules        sandboxpolicy.FilteredNetworkRuleSet
	DNSUpstreams []string
	DNSHosts     map[string][]netip.Addr
	DNSBroker    *filteredNetworkDNSBroker
	DNSWait      <-chan error
}

func encodeFilteredNetworkRelayPolicy(plan sandboxpolicy.MountPlan) (string, error) {
	if plan.NetworkPosture != sandboxpolicy.NetworkFiltered ||
		plan.FilteredNetwork == nil {
		return "", fmt.Errorf("filtered relay requires a compiled network plan")
	}
	if _, err := sandboxpolicy.RenderFilteredNetworkNFT(*plan.FilteredNetwork); err != nil {
		return "", err
	}
	data, err := json.Marshal(plan.FilteredNetwork)
	if err != nil {
		return "", fmt.Errorf("encode filtered network policy: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	if len(encoded) > filteredNetworkPolicyEncodingLimit {
		return "", fmt.Errorf("filtered network policy exceeds the relay encoding limit")
	}
	return encoded, nil
}

func prepareFilteredNetworkRelay(encoded string) (_ preparedFilteredNetworkRelay, retErr error) {
	if strings.TrimSpace(encoded) == "" {
		return preparedFilteredNetworkRelay{}, nil
	}
	if len(encoded) > filteredNetworkPolicyEncodingLimit {
		return preparedFilteredNetworkRelay{}, fmt.Errorf("filtered network policy exceeds the relay encoding limit")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return preparedFilteredNetworkRelay{}, fmt.Errorf("decode filtered network policy: %w", err)
	}
	var ir sandboxpolicy.FilteredNetworkRuleSet
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ir); err != nil {
		return preparedFilteredNetworkRelay{}, fmt.Errorf("parse filtered network policy: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return preparedFilteredNetworkRelay{}, err
	}
	policy, err := sandboxpolicy.RenderFilteredNetworkNFT(ir)
	if err != nil {
		return preparedFilteredNetworkRelay{}, fmt.Errorf("validate filtered network policy: %w", err)
	}
	executables, err := resolveFilteredNetworkExecutables()
	if err != nil {
		return preparedFilteredNetworkRelay{}, err
	}
	hostHosts, err := os.ReadFile("/etc/hosts")
	if err != nil && !os.IsNotExist(err) {
		return preparedFilteredNetworkRelay{}, fmt.Errorf("read host /etc/hosts: %w", err)
	}
	dnsHosts, err := parseFilteredNetworkHostMappings(hostHosts)
	if err != nil {
		return preparedFilteredNetworkRelay{}, err
	}
	hosts, err := sandboxpolicy.FilteredNetworkHostsFile(hostHosts)
	if err != nil {
		return preparedFilteredNetworkRelay{}, err
	}
	resolvConf, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return preparedFilteredNetworkRelay{}, fmt.Errorf(
			"read host /etc/resolv.conf: %w", err)
	}
	resolvDestination, resolvDirs, err := filteredNetworkResolvMount(
		"/etc/resolv.conf", "/run")
	if err != nil {
		return preparedFilteredNetworkRelay{}, err
	}
	dnsUpstreams, err := parseFilteredNetworkDNSUpstreams(resolvConf)
	if err != nil {
		return preparedFilteredNetworkRelay{}, err
	}
	pidDir, err := os.MkdirTemp("", "tclaude-filtered-pasta-")
	if err != nil {
		return preparedFilteredNetworkRelay{}, fmt.Errorf("create pasta readiness directory: %w", err)
	}
	if err := os.Chmod(pidDir, 0o700); err != nil {
		_ = os.RemoveAll(pidDir)
		return preparedFilteredNetworkRelay{}, fmt.Errorf("secure pasta readiness directory: %w", err)
	}
	syncHostPath := filepath.Join(pidDir, "bootstrap.sock")
	syncListener, err := net.ListenUnix(
		"unixpacket",
		&net.UnixAddr{Name: syncHostPath, Net: "unixpacket"},
	)
	if err != nil {
		_ = os.RemoveAll(pidDir)
		return preparedFilteredNetworkRelay{}, fmt.Errorf(
			"create filtered-network readiness listener: %w", err)
	}
	if err := os.Chmod(syncHostPath, 0o600); err != nil {
		_ = syncListener.Close()
		_ = os.RemoveAll(pidDir)
		return preparedFilteredNetworkRelay{}, fmt.Errorf(
			"secure filtered-network readiness listener: %w", err)
	}
	files := []*os.File{}
	closePrepared := func() {
		_ = syncListener.Close()
		for _, file := range files {
			_ = file.Close()
		}
		_ = os.RemoveAll(pidDir)
	}
	defer func() {
		if retErr != nil {
			closePrepared()
		}
	}()
	self, err := os.Open("/proc/self/exe")
	if err != nil {
		return preparedFilteredNetworkRelay{}, fmt.Errorf("open filtered-network bootstrap image: %w", err)
	}
	files = append(files, self)
	policyFile, err := sealedFilteredNetworkData(
		"tclaude-filtered-policy", []byte(policy), 0o400)
	if err != nil {
		return preparedFilteredNetworkRelay{}, err
	}
	files = append(files, policyFile)
	hostsFile, err := sealedFilteredNetworkData(
		"tclaude-filtered-hosts", hosts, 0o444)
	if err != nil {
		return preparedFilteredNetworkRelay{}, err
	}
	files = append(files, hostsFile)
	resolvFile, err := sealedFilteredNetworkData(
		"tclaude-filtered-resolv", sandboxpolicy.FilteredNetworkResolvConf(), 0o444)
	if err != nil {
		return preparedFilteredNetworkRelay{}, err
	}
	files = append(files, resolvFile)
	setupArgs := []string{
		"--ro-bind", syncHostPath, filteredNetworkBootstrapSyncPath,
		"--perms", "0500",
		"--file", strconv.Itoa(filteredNetworkBootstrapBinaryFD),
		sandboxpolicy.FilteredNetworkBootstrapPath,
		"--perms", "0400",
		"--ro-bind-data", strconv.Itoa(filteredNetworkPolicyFD),
		sandboxpolicy.FilteredNetworkNFTPolicyPath,
		"--perms", "0444",
		"--ro-bind-data", strconv.Itoa(filteredNetworkHostsFD),
		"/etc/hosts",
	}
	setupArgs = appendFilteredNetworkResolvDirs(setupArgs, resolvDirs)
	setupArgs = append(setupArgs,
		"--perms", "0444",
		"--ro-bind-data", strconv.Itoa(filteredNetworkResolvFD),
		resolvDestination,
	)
	setupArgs = append(setupArgs, filteredNetworkBootstrapCapabilityArgs()...)
	return preparedFilteredNetworkRelay{
		SetupArgs: setupArgs,
		Command: []string{
			sandboxpolicy.FilteredNetworkBootstrapPath,
			"session",
			tclaudeLayerFilteredBootstrapCommand,
			"--nft", executables.NFT,
			"--",
		},
		Files:        files,
		SyncListener: syncListener,
		PastaPath:    executables.Pasta,
		PastaPIDFile: filepath.Join(pidDir, "pasta.pid"),
		Rules:        ir,
		DNSUpstreams: dnsUpstreams,
		DNSHosts:     dnsHosts,
	}, nil
}

func filteredNetworkResolvMount(
	path string,
	runtimeRoot string,
) (destination string, createDirs []string, err error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", nil, fmt.Errorf(
			"inspect host resolver path %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return filepath.Clean(path), nil, nil
	}
	destination, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, fmt.Errorf(
			"resolve host resolver symlink %s: %w", path, err)
	}
	destination = filepath.Clean(destination)
	runtimeRoot = filepath.Clean(runtimeRoot)
	configRoot := filepath.Dir(filepath.Clean(path))
	switch {
	case sandboxpolicy.PathContainsOrEqual(configRoot, destination):
		return destination, nil, nil
	case sandboxpolicy.PathContainsOrEqual(runtimeRoot, destination):
	default:
		return "", nil, fmt.Errorf(
			"host resolver symlink %s resolves outside supported %s or %s roots: %s",
			path, configRoot, runtimeRoot, destination)
	}
	parent := filepath.Dir(destination)
	relative, err := filepath.Rel(runtimeRoot, parent)
	if err != nil || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", nil, fmt.Errorf(
			"host resolver target %s escapes runtime root %s",
			destination, runtimeRoot)
	}
	createDirs = append(createDirs, runtimeRoot)
	if relative != "." {
		current := runtimeRoot
		for _, part := range strings.Split(relative, string(filepath.Separator)) {
			current = filepath.Join(current, part)
			createDirs = append(createDirs, current)
		}
	}
	return destination, createDirs, nil
}

func appendFilteredNetworkResolvDirs(
	args []string,
	dirs []string,
) []string {
	for index, dir := range dirs {
		if index == 0 {
			// The main bubblewrap plan already created this private runtime
			// filesystem before any authored Unix-socket binds beneath it.
			continue
		}
		args = append(args, "--dir", dir)
	}
	return args
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("parse filtered network policy trailer: %w", err)
	}
	return fmt.Errorf("filtered network policy has trailing JSON data")
}

func sealedFilteredNetworkData(name string, data []byte, mode os.FileMode) (*os.File, error) {
	fd, err := unix.MemfdCreate(name, unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("create sealed %s: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	fail := func(err error) (*os.File, error) {
		_ = file.Close()
		return nil, err
	}
	if err := file.Chmod(mode); err != nil {
		return fail(fmt.Errorf("set sealed %s mode: %w", name, err))
	}
	if _, err := file.Write(data); err != nil {
		return fail(fmt.Errorf("write sealed %s: %w", name, err))
	}
	const seals = unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		return fail(fmt.Errorf("seal %s: %w", name, err))
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fail(fmt.Errorf("rewind sealed %s: %w", name, err))
	}
	return file, nil
}

func (p *preparedFilteredNetworkRelay) Close() {
	if p == nil {
		return
	}
	if p.Sync != nil {
		_ = p.Sync.Close()
	}
	if p.SyncListener != nil {
		_ = p.SyncListener.Close()
	}
	if p.DNSBroker != nil {
		p.DNSBroker.Close()
	}
	for _, file := range p.Files {
		_ = file.Close()
	}
	if p.PastaPIDFile != "" {
		_ = os.RemoveAll(filepath.Dir(p.PastaPIDFile))
	}
}

func (p *preparedFilteredNetworkRelay) waitPolicyReady(namespacePID int) error {
	if p == nil || p.SyncListener == nil {
		return nil
	}
	if err := p.SyncListener.SetDeadline(
		time.Now().Add(filteredNetworkPastaReadyTimeout),
	); err != nil {
		return err
	}
	sync, err := p.SyncListener.AcceptUnix()
	if err != nil {
		return fmt.Errorf("accept filtered-network bootstrap readiness: %w", err)
	}
	p.Sync = sync
	_ = p.SyncListener.Close()
	p.SyncListener = nil
	_ = os.Remove(filepath.Join(filepath.Dir(p.PastaPIDFile), "bootstrap.sock"))
	if err := validateFilteredNetworkSyncPeer(sync, namespacePID); err != nil {
		return err
	}
	if err := sync.SetReadDeadline(time.Now().Add(filteredNetworkPastaReadyTimeout)); err != nil {
		return err
	}
	buffer := make([]byte, 128)
	oob := make([]byte, unix.CmsgSpace(filteredNetworkDNSDescriptorCount*4))
	n, oobn, flags, _, err := sync.ReadMsgUnix(buffer, oob)
	if err != nil {
		return fmt.Errorf("wait for filtered-network nft policy: %w", err)
	}
	if flags&(unix.MSG_CTRUNC|unix.MSG_TRUNC) != 0 {
		return fmt.Errorf("filtered-network bootstrap readiness was truncated")
	}
	if string(buffer[:n]) != sandboxpolicy.FilteredNetworkBootstrapReady {
		return fmt.Errorf("filtered-network bootstrap returned invalid readiness")
	}
	files, err := receiveFilteredNetworkDNSDescriptors(oob[:oobn])
	if err != nil {
		return err
	}
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	packetConn, err := net.FilePacketConn(files[0])
	if err != nil {
		return fmt.Errorf("adopt filtered DNS UDP listener: %w", err)
	}
	udp, ok := packetConn.(*net.UDPConn)
	if !ok {
		_ = packetConn.Close()
		return fmt.Errorf("filtered DNS UDP descriptor has the wrong socket type")
	}
	listener, err := net.FileListener(files[1])
	if err != nil {
		_ = udp.Close()
		return fmt.Errorf("adopt filtered DNS TCP listener: %w", err)
	}
	tcp, ok := listener.(*net.TCPListener)
	if !ok {
		_ = udp.Close()
		_ = listener.Close()
		return fmt.Errorf("filtered DNS TCP descriptor has the wrong socket type")
	}
	nftAuthority, err := duplicateFilteredNetworkFile(files[2], "filtered-dns-nft")
	if err != nil {
		_ = udp.Close()
		_ = tcp.Close()
		return err
	}
	broker, err := newFilteredNetworkDNSBroker(
		p.Rules, udp, tcp, nftAuthority,
		hostFilteredDNSExchange(p.DNSUpstreams, p.DNSHosts),
	)
	if err != nil {
		_ = udp.Close()
		_ = tcp.Close()
		_ = nftAuthority.Close()
		return err
	}
	p.DNSBroker = broker
	p.DNSWait = broker.Start()
	return sync.SetReadDeadline(time.Time{})
}

func receiveFilteredNetworkDNSDescriptors(oob []byte) ([]*os.File, error) {
	messages, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, fmt.Errorf("parse filtered DNS descriptor handoff: %w", err)
	}
	var descriptors []int
	for _, message := range messages {
		rights, rightsErr := unix.ParseUnixRights(&message)
		if rightsErr != nil {
			for _, descriptor := range descriptors {
				_ = unix.Close(descriptor)
			}
			return nil, fmt.Errorf(
				"parse filtered DNS descriptor rights: %w", rightsErr)
		}
		descriptors = append(descriptors, rights...)
	}
	if len(descriptors) != filteredNetworkDNSDescriptorCount {
		for _, descriptor := range descriptors {
			_ = unix.Close(descriptor)
		}
		return nil, fmt.Errorf(
			"filtered DNS descriptor handoff has %d descriptors (want %d)",
			len(descriptors), filteredNetworkDNSDescriptorCount,
		)
	}
	files := make([]*os.File, 0, len(descriptors))
	for index, descriptor := range descriptors {
		files = append(files, os.NewFile(
			uintptr(descriptor), fmt.Sprintf("filtered-dns-%d", index)))
	}
	return files, nil
}

func duplicateFilteredNetworkFile(source *os.File, name string) (*os.File, error) {
	descriptor, err := unix.Dup(int(source.Fd()))
	if err != nil {
		return nil, fmt.Errorf("duplicate %s descriptor: %w", name, err)
	}
	unix.CloseOnExec(descriptor)
	return os.NewFile(uintptr(descriptor), name), nil
}

func validateFilteredNetworkSyncPeer(conn *net.UnixConn, namespacePID int) error {
	if conn == nil || namespacePID <= 0 {
		return fmt.Errorf("filtered-network readiness peer contract is invalid")
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("inspect filtered-network readiness peer: %w", err)
	}
	var credential *unix.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credential, socketErr = unix.GetsockoptUcred(
			int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return fmt.Errorf("inspect filtered-network readiness socket: %w", err)
	}
	if socketErr != nil {
		return fmt.Errorf("authenticate filtered-network readiness peer: %w", socketErr)
	}
	if credential == nil || credential.Pid <= 0 {
		return fmt.Errorf("filtered-network readiness peer has no process identity")
	}
	expectedNamespace, err := os.Readlink(
		filepath.Join("/proc", strconv.Itoa(namespacePID), "ns/net"))
	if err != nil {
		return fmt.Errorf("inspect filtered-network namespace identity: %w", err)
	}
	peerNamespace, err := os.Readlink(
		filepath.Join("/proc", strconv.Itoa(int(credential.Pid)), "ns/net"))
	if err != nil {
		return fmt.Errorf("inspect filtered-network readiness peer namespace: %w", err)
	}
	if peerNamespace != expectedNamespace {
		return fmt.Errorf("filtered-network readiness peer is outside the sandbox network namespace")
	}
	return nil
}

func (p *preparedFilteredNetworkRelay) releaseHarness() error {
	if p == nil || p.Sync == nil {
		return nil
	}
	if _, err := p.Sync.Write([]byte("gateway-ready")); err != nil {
		return fmt.Errorf("release filtered-network harness gate: %w", err)
	}
	return nil
}

func (p *preparedFilteredNetworkRelay) startPasta(
	namespacePID int,
) (*exec.Cmd, <-chan error, error) {
	if p == nil || p.PastaPath == "" {
		return nil, nil, nil
	}
	args := filteredNetworkPastaArgs(p.PastaPIDFile, namespacePID)
	cmd := exec.Command(p.PastaPath, args...)
	cmd.Env = filteredNetworkHelperEnv()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start filtered-network pasta gateway: %w", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	deadline := time.Now().Add(filteredNetworkPastaReadyTimeout)
	for {
		data, err := os.ReadFile(p.PastaPIDFile)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil {
				if pid != cmd.Process.Pid {
					_ = cmd.Process.Kill()
					<-waitCh
					return nil, nil, fmt.Errorf("pasta readiness pid is invalid")
				}
				return cmd, waitCh, nil
			}
		} else if !os.IsNotExist(err) {
			_ = cmd.Process.Kill()
			<-waitCh
			return nil, nil, fmt.Errorf("read pasta readiness: %w", err)
		}
		select {
		case waitErr := <-waitCh:
			return nil, nil, fmt.Errorf("pasta exited before readiness: %w", waitErr)
		default:
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			<-waitCh
			return nil, nil, fmt.Errorf("pasta readiness timed out after %s", filteredNetworkPastaReadyTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func filteredNetworkPastaArgs(pidFile string, namespacePID int) []string {
	return []string{
		"--foreground",
		"--quiet",
		"--config-net",
		// Give the namespace an IPv6 default route to pasta's emulated
		// gateway. The reserved fd00::2 host-loopback mapping must not depend
		// on an unrelated route existing in the host's selected template.
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
		"--pid", pidFile,
		strconv.Itoa(namespacePID),
	}
}

func tclaudeLayerFilteredBootstrapCmd() *cobra.Command {
	var nftPath string
	cmd := &cobra.Command{
		Use:    tclaudeLayerFilteredBootstrapCommand + " -- <command> [args...]",
		Short:  "Install the filtered-network policy and enter the harness (internal)",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runTclaudeLayerFilteredBootstrap(nftPath, args)
		},
	}
	cmd.Flags().StringVar(&nftPath, "nft", "", "resolved nft executable (internal)")
	return cmd
}

func tclaudeLayerFilteredNFTCmd() *cobra.Command {
	var nftPath string
	cmd := &cobra.Command{
		Use:    tclaudeLayerFilteredNFTCommand,
		Short:  "Verify filtered-network nft authority and install policy (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return runTclaudeLayerFilteredNFT(nftPath)
		},
	}
	cmd.Flags().StringVar(&nftPath, "nft", "", "resolved nft executable (internal)")
	return cmd
}

func runTclaudeLayerFilteredBootstrap(nftPath string, command []string) error {
	nftPath = filepath.Clean(strings.TrimSpace(nftPath))
	if !filepath.IsAbs(nftPath) || len(command) == 0 {
		return fmt.Errorf("filtered-network bootstrap contract is invalid")
	}
	dnsFiles, err := openFilteredNetworkDNSDescriptors()
	if err != nil {
		return err
	}
	defer func() {
		for _, file := range dnsFiles {
			_ = file.Close()
		}
	}()
	if err := narrowFilteredNetworkBootstrapForNFT(); err != nil {
		return err
	}
	nft := filteredNetworkNFTCommand(nftPath)
	nft.Stdin = nil
	nft.Stdout = os.Stderr
	nft.Stderr = os.Stderr
	if err := nft.Run(); err != nil {
		return fmt.Errorf("install atomic filtered-network nft policy: %w", err)
	}
	sync, err := net.DialUnix(
		"unixpacket",
		nil,
		&net.UnixAddr{Name: filteredNetworkBootstrapSyncPath, Net: "unixpacket"},
	)
	if err != nil {
		return fmt.Errorf("connect filtered-network readiness channel: %w", err)
	}
	rights := unix.UnixRights(
		int(dnsFiles[0].Fd()), int(dnsFiles[1].Fd()), int(dnsFiles[2].Fd()))
	if _, _, err := sync.WriteMsgUnix(
		[]byte(sandboxpolicy.FilteredNetworkBootstrapReady), rights, nil,
	); err != nil {
		_ = sync.Close()
		return fmt.Errorf("signal filtered-network nft readiness: %w", err)
	}
	buffer := make([]byte, 64)
	n, err := sync.Read(buffer)
	if err != nil {
		_ = sync.Close()
		return fmt.Errorf("wait for filtered-network gateway readiness: %w", err)
	}
	if string(buffer[:n]) != "gateway-ready" {
		_ = sync.Close()
		return fmt.Errorf("filtered-network gateway returned invalid readiness")
	}
	if err := sync.Close(); err != nil {
		return fmt.Errorf("close filtered-network readiness authority: %w", err)
	}
	if err := dropFilteredNetworkBootstrapCapabilities(); err != nil {
		return err
	}
	_ = os.Remove(sandboxpolicy.FilteredNetworkBootstrapPath)
	_ = os.Remove(sandboxpolicy.FilteredNetworkNFTPolicyPath)
	executable := command[0]
	if !filepath.IsAbs(executable) {
		var err error
		executable, err = exec.LookPath(executable)
		if err != nil {
			return fmt.Errorf("resolve filtered-network harness executable: %w", err)
		}
	}
	return unix.Exec(executable, command, os.Environ())
}

type filteredNetworkCapabilityState struct {
	Effective   uint64
	Permitted   uint64
	Inheritable uint64
	Ambient     uint64
}

func narrowFilteredNetworkBootstrapForNFT() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no-new-privileges before filtered nft exec: %w", err)
	}
	if err := unix.Prctl(
		unix.PR_CAP_AMBIENT,
		unix.PR_CAP_AMBIENT_CLEAR_ALL,
		0, 0, 0,
	); err != nil {
		return fmt.Errorf("clear ambient capabilities before filtered nft exec: %w", err)
	}
	mask := uint32(1) << unix.CAP_NET_ADMIN
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{{
		Effective: mask, Permitted: mask, Inheritable: mask,
	}}
	if err := unix.Capset(&header, &data[0]); err != nil {
		return fmt.Errorf("narrow filtered nft capability sets: %w", err)
	}
	expected := filteredNetworkCapabilityState{
		Effective: uint64(mask), Permitted: uint64(mask),
		Inheritable: uint64(mask),
	}
	if err := requireFilteredNetworkCapabilityState(expected); err != nil {
		return fmt.Errorf("verify filtered nft parent capability sets: %w", err)
	}
	return nil
}

func runTclaudeLayerFilteredNFT(nftPath string) error {
	nftPath = filepath.Clean(strings.TrimSpace(nftPath))
	if !filepath.IsAbs(nftPath) {
		return fmt.Errorf("filtered-network nft child contract is invalid")
	}
	mask := uint64(1) << unix.CAP_NET_ADMIN
	expected := filteredNetworkCapabilityState{
		Effective: mask, Permitted: mask, Inheritable: mask, Ambient: mask,
	}
	if err := requireFilteredNetworkCapabilityState(expected); err != nil {
		return fmt.Errorf("verify filtered nft child capability sets: %w", err)
	}
	noNewPrivileges, _, errno := unix.Syscall6(
		unix.SYS_PRCTL,
		uintptr(unix.PR_GET_NO_NEW_PRIVS),
		0, 0, 0, 0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("inspect filtered nft no-new-privileges: %w", errno)
	}
	if noNewPrivileges != 1 {
		return fmt.Errorf("filtered nft child has no-new-privileges disabled")
	}
	return unix.Exec(
		nftPath,
		[]string{nftPath, "-f", sandboxpolicy.FilteredNetworkNFTPolicyPath},
		filteredNetworkHelperEnv(),
	)
}

func requireFilteredNetworkCapabilityState(
	expected filteredNetworkCapabilityState,
) error {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return fmt.Errorf("read filtered-network capability state: %w", err)
	}
	actual, err := parseFilteredNetworkCapabilityState(status)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf(
			"capability state is effective=%016x permitted=%016x inheritable=%016x ambient=%016x; want effective=%016x permitted=%016x inheritable=%016x ambient=%016x",
			actual.Effective,
			actual.Permitted,
			actual.Inheritable,
			actual.Ambient,
			expected.Effective,
			expected.Permitted,
			expected.Inheritable,
			expected.Ambient,
		)
	}
	return nil
}

func parseFilteredNetworkCapabilityState(
	status []byte,
) (filteredNetworkCapabilityState, error) {
	values := map[string]*uint64{}
	var state filteredNetworkCapabilityState
	values["CapEff"] = &state.Effective
	values["CapPrm"] = &state.Permitted
	values["CapInh"] = &state.Inheritable
	values["CapAmb"] = &state.Ambient
	for _, line := range strings.Split(string(status), "\n") {
		name, value, found := strings.Cut(line, ":")
		target, wanted := values[name]
		if !found || !wanted {
			continue
		}
		parsed, parseErr := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
		if parseErr != nil {
			return filteredNetworkCapabilityState{}, fmt.Errorf(
				"parse filtered-network %s: %w", name, parseErr)
		}
		*target = parsed
		delete(values, name)
	}
	if len(values) != 0 {
		missing := make([]string, 0, len(values))
		for name := range values {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return filteredNetworkCapabilityState{}, fmt.Errorf(
			"filtered-network capability state is missing %s",
			strings.Join(missing, ", "),
		)
	}
	return state, nil
}

func openFilteredNetworkDNSDescriptors() ([]*os.File, error) {
	address := net.JoinHostPort(
		sandboxpolicy.FilteredNetworkDNSIPv4,
		strconv.Itoa(filteredNetworkDNSPort),
	)
	udpAddress, err := net.ResolveUDPAddr("udp4", address)
	if err != nil {
		return nil, fmt.Errorf("resolve filtered DNS UDP listener: %w", err)
	}
	udp, err := net.ListenUDP("udp4", udpAddress)
	if err != nil {
		return nil, fmt.Errorf("bind filtered DNS UDP listener: %w", err)
	}
	closeAll := func(files ...*os.File) {
		_ = udp.Close()
		for _, file := range files {
			if file != nil {
				_ = file.Close()
			}
		}
	}
	tcpAddress, err := net.ResolveTCPAddr("tcp4", address)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("resolve filtered DNS TCP listener: %w", err)
	}
	tcp, err := net.ListenTCP("tcp4", tcpAddress)
	if err != nil {
		closeAll()
		return nil, fmt.Errorf("bind filtered DNS TCP listener: %w", err)
	}
	udpFile, err := udp.File()
	if err != nil {
		_ = tcp.Close()
		closeAll()
		return nil, fmt.Errorf("export filtered DNS UDP listener: %w", err)
	}
	tcpFile, err := tcp.File()
	_ = udp.Close()
	_ = tcp.Close()
	if err != nil {
		closeAll(udpFile)
		return nil, fmt.Errorf("export filtered DNS TCP listener: %w", err)
	}
	nftFD, err := unix.Socket(
		unix.AF_NETLINK,
		unix.SOCK_RAW|unix.SOCK_CLOEXEC,
		unix.NETLINK_NETFILTER,
	)
	if err != nil {
		closeAll(udpFile, tcpFile)
		return nil, fmt.Errorf("create filtered DNS nft authority: %w", err)
	}
	if err := unix.Bind(nftFD, &unix.SockaddrNetlink{
		Family: unix.AF_NETLINK,
	}); err != nil {
		_ = unix.Close(nftFD)
		closeAll(udpFile, tcpFile)
		return nil, fmt.Errorf("bind filtered DNS nft authority: %w", err)
	}
	nftFile := os.NewFile(uintptr(nftFD), "filtered-dns-nft")
	return []*os.File{udpFile, tcpFile, nftFile}, nil
}

func dropFilteredNetworkBootstrapCapabilities() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no-new-privileges before filtered harness exec: %w", err)
	}
	_ = unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0)
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capset(&header, &data[0]); err != nil {
		return fmt.Errorf("drop filtered-network bootstrap capabilities: %w", err)
	}
	current := [2]unix.CapUserData{}
	if err := unix.Capget(&header, &current[0]); err != nil {
		return fmt.Errorf("verify filtered-network capability drop: %w", err)
	}
	for _, set := range current {
		if set.Effective != 0 || set.Permitted != 0 || set.Inheritable != 0 {
			return fmt.Errorf("filtered-network bootstrap retained Linux capabilities")
		}
	}
	return nil
}

func filteredNetworkRelayPrefix(plan sandboxpolicy.MountPlan) (string, error) {
	if plan.NetworkPosture != sandboxpolicy.NetworkFiltered {
		return "", nil
	}
	encoded, err := encodeFilteredNetworkRelayPolicy(plan)
	if err != nil {
		return "", err
	}
	return " --filtered-network-policy " + clcommon.ShellQuoteArg(encoded), nil
}
