//go:build linux

package session

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	filteredNetworkPolicyEncodingLimit   = 64 << 10
	filteredNetworkSyncChildFD           = 4
	filteredNetworkBootstrapBinaryFD     = 5
	filteredNetworkPolicyFD              = 6
	filteredNetworkHostsFD               = 7
	filteredNetworkPastaReadyTimeout     = 5 * time.Second
)

func filteredNetworkHelperEnv() []string {
	return []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C",
		"LC_ALL=C",
	}
}

type preparedFilteredNetworkRelay struct {
	SetupArgs    []string
	Command      []string
	Files        []*os.File
	Sync         *os.File
	PastaPath    string
	PastaPIDFile string
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
	hosts, err := os.ReadFile("/etc/hosts")
	if err != nil && !os.IsNotExist(err) {
		return preparedFilteredNetworkRelay{}, fmt.Errorf("read host /etc/hosts: %w", err)
	}
	hosts, err = sandboxpolicy.FilteredNetworkHostsFile(hosts)
	if err != nil {
		return preparedFilteredNetworkRelay{}, err
	}
	sockets, err := unix.Socketpair(
		unix.AF_UNIX,
		unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC,
		0,
	)
	if err != nil {
		return preparedFilteredNetworkRelay{}, fmt.Errorf("create filtered-network readiness channel: %w", err)
	}
	parentSync := os.NewFile(uintptr(sockets[0]), "filtered-network-parent")
	childSync := os.NewFile(uintptr(sockets[1]), "filtered-network-child")
	files := []*os.File{childSync}
	closePrepared := func() {
		_ = parentSync.Close()
		for _, file := range files {
			_ = file.Close()
		}
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
	pidDir, err := os.MkdirTemp("", "tclaude-filtered-pasta-")
	if err != nil {
		return preparedFilteredNetworkRelay{}, fmt.Errorf("create pasta readiness directory: %w", err)
	}
	if err := os.Chmod(pidDir, 0o700); err != nil {
		_ = os.RemoveAll(pidDir)
		return preparedFilteredNetworkRelay{}, fmt.Errorf("secure pasta readiness directory: %w", err)
	}
	return preparedFilteredNetworkRelay{
		SetupArgs: []string{
			"--sync-fd", strconv.Itoa(filteredNetworkSyncChildFD),
			"--perms", "0500",
			"--file", strconv.Itoa(filteredNetworkBootstrapBinaryFD),
			sandboxpolicy.FilteredNetworkBootstrapPath,
			"--perms", "0400",
			"--ro-bind-data", strconv.Itoa(filteredNetworkPolicyFD),
			sandboxpolicy.FilteredNetworkNFTPolicyPath,
			"--perms", "0444",
			"--ro-bind-data", strconv.Itoa(filteredNetworkHostsFD),
			"/etc/hosts",
			"--cap-add", "CAP_NET_ADMIN",
		},
		Command: []string{
			sandboxpolicy.FilteredNetworkBootstrapPath,
			"session",
			tclaudeLayerFilteredBootstrapCommand,
			"--nft", executables.NFT,
			"--",
		},
		Files:        files,
		Sync:         parentSync,
		PastaPath:    executables.Pasta,
		PastaPIDFile: filepath.Join(pidDir, "pasta.pid"),
	}, nil
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
	for _, file := range p.Files {
		_ = file.Close()
	}
	if p.PastaPIDFile != "" {
		_ = os.RemoveAll(filepath.Dir(p.PastaPIDFile))
	}
}

func (p *preparedFilteredNetworkRelay) waitPolicyReady() error {
	if p == nil || p.Sync == nil {
		return nil
	}
	if err := p.Sync.SetReadDeadline(time.Now().Add(filteredNetworkPastaReadyTimeout)); err != nil {
		return err
	}
	buffer := make([]byte, 128)
	n, err := p.Sync.Read(buffer)
	if err != nil {
		return fmt.Errorf("wait for filtered-network nft policy: %w", err)
	}
	if string(buffer[:n]) != sandboxpolicy.FilteredNetworkBootstrapReady {
		return fmt.Errorf("filtered-network bootstrap returned invalid readiness")
	}
	return p.Sync.SetReadDeadline(time.Time{})
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
	args := []string{
		"--foreground",
		"--quiet",
		"--config-net",
		"--no-map-gw",
		"--map-guest-addr", "none",
		"--map-host-loopback", sandboxpolicy.FilteredNetworkLoopbackIPv4,
		"--map-host-loopback", sandboxpolicy.FilteredNetworkLoopbackIPv6,
		"--tcp-ports", "none",
		"--udp-ports", "none",
		"--tcp-ns", "none",
		"--udp-ns", "none",
		"--no-splice",
		"--pid", p.PastaPIDFile,
		strconv.Itoa(namespacePID),
	}
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
			if parseErr != nil || pid != cmd.Process.Pid {
				_ = cmd.Process.Kill()
				<-waitCh
				return nil, nil, fmt.Errorf("pasta readiness pid is invalid")
			}
			return cmd, waitCh, nil
		}
		if !os.IsNotExist(err) {
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

func runTclaudeLayerFilteredBootstrap(nftPath string, command []string) error {
	nftPath = filepath.Clean(strings.TrimSpace(nftPath))
	if !filepath.IsAbs(nftPath) || len(command) == 0 {
		return fmt.Errorf("filtered-network bootstrap contract is invalid")
	}
	nft := exec.Command(nftPath, "-f", sandboxpolicy.FilteredNetworkNFTPolicyPath)
	nft.Env = filteredNetworkHelperEnv()
	nft.Stdin = nil
	nft.Stdout = os.Stderr
	nft.Stderr = os.Stderr
	if err := nft.Run(); err != nil {
		return fmt.Errorf("install atomic filtered-network nft policy: %w", err)
	}
	sync := os.NewFile(uintptr(filteredNetworkSyncChildFD), "filtered-network-sync")
	if sync == nil {
		return fmt.Errorf("filtered-network readiness channel is unavailable")
	}
	if _, err := sync.Write([]byte(sandboxpolicy.FilteredNetworkBootstrapReady)); err != nil {
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
