//go:build linux

package agentd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const (
	linuxRouteM6EnabledEnv = "TCLAUDE_LINUX_ROUTE_CAPABILITY_SMOKE"
	linuxRouteM6ChildEnv   = "TCL952_LINUX_ROUTE_CHILD"
	linuxRouteM6RoleEnv    = "TCL952_LINUX_ROUTE_ROLE"
	linuxRouteM6SocketEnv  = "TCL952_LINUX_ROUTE_SOCKET"
	linuxRouteM6ControlEnv = "TCL952_LINUX_ROUTE_CONTROL"
	linuxRouteM6ReadyEnv   = "TCL952_LINUX_ROUTE_READY"
	linuxRouteM6StopEnv    = "TCL952_LINUX_ROUTE_STOP"
	linuxRouteM6CredEnv    = "TCL952_LINUX_ROUTE_CREDENTIAL"
	linuxRouteM6AgentEnv   = "TCL952_LINUX_ROUTE_AGENT"
	linuxRouteM6ConvEnv    = "TCL952_LINUX_ROUTE_CONV"
	linuxRouteM6GenEnv     = "TCL952_LINUX_ROUTE_GENERATION"
	linuxRouteM6RouteEnv   = "TCL952_LINUX_ROUTE_DESCRIPTOR"
	linuxRouteM6LeaseEnv   = "TCL952_LINUX_ROUTE_LEASE"
	linuxRouteM6HostEnv    = "TCL952_LINUX_ROUTE_HOST_PORT"
	linuxRouteM6Opaque     = "tcl952-linux-opaque"
	linuxRouteM6Count      = 96
	linuxRouteM6HelperDir  = "/run/tcl952-route"
	linuxRouteM6HelperPath = linuxRouteM6HelperDir + "/helper"
)

// The child's stage budget must stay strictly under the host's marker deadline
// so a stalled boundary reports its own named cause before the host gives up on
// an anonymous timeout. TCL-960's original failure had no such attribution: the
// helper simply went quiet for 30 seconds.
const (
	linuxRouteM6MarkerTimeout     = 30 * time.Second
	linuxRouteM6DescriptorTimeout = 15 * time.Second
	linuxRouteM6AttachTimeout     = 10 * time.Second
)

func parseLinuxRouteM6PublisherDescriptor(raw string) (string, int64, error) {
	parts := strings.Split(strings.TrimSpace(raw), "|")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
		return "", 0, fmt.Errorf("publisher route descriptor must be route-id|group-generation")
	}
	groupGeneration, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("publisher route descriptor group generation: %w", err)
	}
	return strings.TrimSpace(parts[0]), groupGeneration, nil
}

func TestLinuxRouteM6PublisherDescriptorSchema(t *testing.T) {
	routeID, generation, err := parseLinuxRouteM6PublisherDescriptor("rte_test|7")
	require.NoError(t, err)
	require.Equal(t, "rte_test", routeID)
	require.EqualValues(t, 7, generation)
	_, _, err = parseLinuxRouteM6PublisherDescriptor("rte_test|7|extra")
	require.Error(t, err)
	_, _, err = parseLinuxRouteM6PublisherDescriptor("rte_test|not-a-generation")
	require.Error(t, err)
}

// linuxRouteM6ControlPaths are the control-directory files a namespace child
// reads or writes. The child shares no memory with the host cell, so every path
// it needs has to be handed over explicitly through the environment. TCL-960
// diagnosed the M6 Linux stall as exactly this hand-off going missing: the
// descriptor and lease variables were read by the child but never set, so the
// publisher waited on an empty path and timed out before it ever dialled.
type linuxRouteM6ControlPaths struct {
	Ready      string
	Stop       string
	Descriptor string
	Lease      string
}

func linuxRouteM6PathsFor(control, role string) linuxRouteM6ControlPaths {
	return linuxRouteM6ControlPaths{
		Ready:      filepath.Join(control, role+".ready"),
		Stop:       filepath.Join(control, role+".stop"),
		Descriptor: filepath.Join(control, role+".route"),
		Lease:      filepath.Join(control, role+".lease"),
	}
}

// linuxRouteM6ChildEnvironment is kept pure so the hand-off contract can be
// asserted on any Linux runner, including ones without Bubblewrap.
func linuxRouteM6ChildEnvironment(base []string, role, socketPath, control, credential, agentID, convID, generation string, hostPort int) []string {
	paths := linuxRouteM6PathsFor(control, role)
	return append(base,
		linuxRouteM6ChildEnv+"=1",
		linuxRouteM6RoleEnv+"="+role,
		linuxRouteM6SocketEnv+"="+socketPath,
		linuxRouteM6ControlEnv+"="+control,
		linuxRouteM6ReadyEnv+"="+paths.Ready,
		linuxRouteM6StopEnv+"="+paths.Stop,
		linuxRouteM6RouteEnv+"="+paths.Descriptor,
		linuxRouteM6LeaseEnv+"="+paths.Lease,
		linuxRouteM6CredEnv+"="+credential,
		linuxRouteM6AgentEnv+"="+agentID,
		linuxRouteM6ConvEnv+"="+convID,
		linuxRouteM6GenEnv+"="+generation,
		linuxRouteM6HostEnv+"="+strconv.Itoa(hostPort),
	)
}

// TestLinuxRouteM6ChildEnvironmentHandsOverEveryReadPath is the regression guard
// for TCL-960. It runs in ordinary CI without Bubblewrap, so a future edit that
// adds a control file the child reads but the host forgets to pass fails here
// instead of as an unattributable timeout inside the namespace.
func TestLinuxRouteM6ChildEnvironmentHandsOverEveryReadPath(t *testing.T) {
	const control = "/tmp/tcl952-route-control"
	for _, role := range []string{"publisher", "consumer"} {
		env := linuxRouteM6ChildEnvironment(nil, role, "/tmp/agentd.sock", control, "cred", "agt_1", "conv-1", "gen-1", 4321)
		values := map[string]string{}
		for _, entry := range env {
			key, value, ok := strings.Cut(entry, "=")
			require.True(t, ok, "child environment entry must be key=value: %q", entry)
			values[key] = value
		}
		// Every variable the child reads must arrive with a usable value.
		for _, key := range []string{
			linuxRouteM6ChildEnv, linuxRouteM6RoleEnv, linuxRouteM6SocketEnv, linuxRouteM6ControlEnv,
			linuxRouteM6ReadyEnv, linuxRouteM6StopEnv, linuxRouteM6RouteEnv, linuxRouteM6LeaseEnv,
			linuxRouteM6CredEnv, linuxRouteM6AgentEnv, linuxRouteM6ConvEnv, linuxRouteM6GenEnv, linuxRouteM6HostEnv,
		} {
			value, present := values[key]
			require.True(t, present, "role %s must receive %s", role, key)
			require.NotEmpty(t, strings.TrimSpace(value), "role %s must receive a non-empty %s", role, key)
		}
		paths := linuxRouteM6PathsFor(control, role)
		require.Equal(t, paths.Descriptor, values[linuxRouteM6RouteEnv])
		require.Equal(t, paths.Lease, values[linuxRouteM6LeaseEnv])
		for _, key := range []string{linuxRouteM6ReadyEnv, linuxRouteM6StopEnv, linuxRouteM6RouteEnv, linuxRouteM6LeaseEnv} {
			require.Equal(t, control, filepath.Dir(values[key]), "%s must live in the bound control directory", key)
		}
	}
	// The host writes these exact names; a rename on either side must not go
	// unnoticed, because the child would then wait on a file nobody creates.
	require.Equal(t, filepath.Join(control, "publisher.route"), linuxRouteM6PathsFor(control, "publisher").Descriptor)
	require.Equal(t, filepath.Join(control, "consumer.lease"), linuxRouteM6PathsFor(control, "consumer").Lease)
}

// TestLinuxRouteM6Child is run only by the dedicated exact-head workflow. The
// child executes the production routeadapter in a real unshared Bubblewrap
// network namespace; the only test seam is the child process itself.
func TestLinuxRouteM6Child(t *testing.T) {
	if os.Getenv(linuxRouteM6ChildEnv) != "1" {
		t.Skip("route smoke helper subprocess")
	}
	switch os.Getenv(linuxRouteM6RoleEnv) {
	case "publisher":
		linuxRouteM6Publisher(t)
	case "consumer":
		linuxRouteM6Consumer(t)
	default:
		t.Fatalf("unknown Linux route smoke role %q", os.Getenv(linuxRouteM6RoleEnv))
	}
}

func linuxRouteM6Publisher(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	target := "tcp://" + listener.Addr().String()
	linuxRouteM6Write(t, "target="+target)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				for {
					payload := make([]byte, len(linuxRouteM6Opaque))
					if _, err := io.ReadFull(conn, payload); err != nil {
						return
					}
					if string(payload) != linuxRouteM6Opaque {
						return
					}
					if _, err := conn.Write([]byte("reply:" + linuxRouteM6Opaque)); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	// These probes are intentionally made from the publisher namespace. A
	// successful connection would prove that the route capability widened the
	// existing host/Internet floor and must fail the activation cell.
	hostPort := os.Getenv(linuxRouteM6HostEnv)
	hostConn, hostErr := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", hostPort), 300*time.Millisecond)
	if hostConn != nil {
		_ = hostConn.Close()
	}
	if hostErr == nil {
		linuxRouteM6Write(t, "policy-floor:host-reachable")
		t.Fatalf("publisher namespace reached host control listener")
	}
	internetConn, internetErr := net.DialTimeout("tcp", "1.1.1.1:443", 300*time.Millisecond)
	if internetConn != nil {
		_ = internetConn.Close()
	}
	if internetErr == nil {
		linuxRouteM6Write(t, "policy-floor:internet-reachable")
		t.Fatalf("publisher namespace reached Internet despite unshared route smoke floor")
	}
	linuxRouteM6Write(t, "policy-floor:host-and-internet-denied")

	linuxRouteM6Stage(t, "descriptor-wait")
	descriptor, err := linuxRouteM6WaitFile(os.Getenv(linuxRouteM6RouteEnv), linuxRouteM6DescriptorTimeout)
	if err != nil {
		linuxRouteM6StageFailed(t, "descriptor-wait", err)
	}
	routeID, groupGeneration, err := parseLinuxRouteM6PublisherDescriptor(descriptor)
	if err != nil {
		linuxRouteM6StageFailed(t, "descriptor-parse", err)
	}
	linuxRouteM6Stage(t, "descriptor-ok:"+routeID)

	// The dial covers credential presentation, the Unix socket, and the HTTP
	// upgrade in one production call, so it is bounded and reported as its own
	// stage rather than left to expire against the host's marker deadline.
	linuxRouteM6Stage(t, "channel-dial")
	dialCtx, cancelDial := context.WithTimeout(context.Background(), linuxRouteM6AttachTimeout)
	defer cancelDial()
	channel, err := routeadapter.DialUnixChannel(dialCtx, os.Getenv(linuxRouteM6SocketEnv), routeadapter.ChannelAuth{
		Role:             routeadapter.RolePublisher,
		RouteID:          routeID,
		AgentID:          os.Getenv(linuxRouteM6AgentEnv),
		ConvID:           os.Getenv(linuxRouteM6ConvEnv),
		LaunchGeneration: os.Getenv(linuxRouteM6GenEnv),
		GroupGeneration:  groupGeneration,
		Credential:       os.Getenv(linuxRouteM6CredEnv),
	})
	if err != nil {
		linuxRouteM6StageFailed(t, "channel-dial", err)
	}
	linuxRouteM6Write(t, "channel-attached")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- routeadapter.RunPublisher(ctx, channel, target) }()
	select {
	case err := <-done:
		if err != nil {
			linuxRouteM6Write(t, "channel-closed:"+err.Error())
			t.Fatal(err)
		}
		linuxRouteM6Write(t, "channel-closed")
	case <-linuxRouteM6WaitStop(os.Getenv(linuxRouteM6StopEnv)):
		cancel()
		err := <-done
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		linuxRouteM6Write(t, "channel-closed")
	}
}

func linuxRouteM6Consumer(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	endpoint := "tcp://" + listener.Addr().String()
	linuxRouteM6Write(t, "listener="+endpoint)
	linuxRouteM6Stage(t, "lease-wait")
	descriptor, err := linuxRouteM6WaitFile(os.Getenv(linuxRouteM6LeaseEnv), linuxRouteM6DescriptorTimeout)
	if err != nil {
		linuxRouteM6StageFailed(t, "lease-wait", err)
	}
	parts := strings.Split(strings.TrimSpace(descriptor), "|")
	if len(parts) != 3 {
		linuxRouteM6StageFailed(t, "lease-parse", fmt.Errorf("consumer lease descriptor must be lease-id|route-id|group-generation, got %q", descriptor))
	}
	groupGeneration, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		linuxRouteM6StageFailed(t, "lease-parse", err)
	}
	linuxRouteM6Stage(t, "lease-ok:"+parts[0])

	// The endpoint status call is the authenticated credential/Unix-socket
	// boundary the consumer crosses before any data channel exists; report it
	// separately so a credential refusal is never mistaken for an attach stall.
	linuxRouteM6Stage(t, "endpoint-status")
	if err := linuxRouteM6SetEndpointStatus(
		os.Getenv(linuxRouteM6SocketEnv), parts[0], endpoint,
		os.Getenv(linuxRouteM6CredEnv), os.Getenv(linuxRouteM6AgentEnv), os.Getenv(linuxRouteM6ConvEnv), os.Getenv(linuxRouteM6GenEnv)); err != nil {
		linuxRouteM6StageFailed(t, "endpoint-status", err)
	}
	linuxRouteM6Stage(t, "channel-dial")
	dialCtx, cancelDial := context.WithTimeout(context.Background(), linuxRouteM6AttachTimeout)
	defer cancelDial()
	channel, err := routeadapter.DialUnixChannel(dialCtx, os.Getenv(linuxRouteM6SocketEnv), routeadapter.ChannelAuth{
		Role:             routeadapter.RoleConsumer,
		RouteID:          parts[1],
		LeaseID:          parts[0],
		AgentID:          os.Getenv(linuxRouteM6AgentEnv),
		ConvID:           os.Getenv(linuxRouteM6ConvEnv),
		LaunchGeneration: os.Getenv(linuxRouteM6GenEnv),
		GroupGeneration:  groupGeneration,
		Credential:       os.Getenv(linuxRouteM6CredEnv),
		ConsumerEndpoint: endpoint,
	})
	if err != nil {
		linuxRouteM6StageFailed(t, "channel-dial", err)
	}
	linuxRouteM6Write(t, "channel-attached")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- routeadapter.RunConsumer(ctx, channel, listener) }()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	for i := 0; i < linuxRouteM6Count; i++ {
		conn, dialErr := net.DialTimeout("tcp", net.JoinHostPort(host, port), time.Second)
		require.NoError(t, dialErr)
		_, err = conn.Write([]byte(linuxRouteM6Opaque))
		require.NoError(t, err)
		response := make([]byte, len("reply:")+len(linuxRouteM6Opaque))
		_, err = io.ReadFull(conn, response)
		_ = conn.Close()
		require.NoError(t, err)
		require.Equal(t, "reply:"+linuxRouteM6Opaque, string(response))
		time.Sleep(5 * time.Millisecond)
	}
	linuxRouteM6Write(t, fmt.Sprintf("sustained-route-traffic:%d", linuxRouteM6Count))
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		linuxRouteM6Write(t, "channel-closed")
	case <-linuxRouteM6WaitStop(os.Getenv(linuxRouteM6StopEnv)):
		cancel()
		err := <-done
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		linuxRouteM6Write(t, "channel-closed")
	}
}

func linuxRouteM6SetEndpointStatus(socketPath, leaseID, endpoint, credential, agentID, convID, generation string) error {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	payload, _ := json.Marshal(map[string]string{"state": "ready", "endpoint": endpoint})
	req, err := http.NewRequest(http.MethodPost, "http://tclaude.invalid/v1/routes/leases/"+leaseID+"/endpoint", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tclaude-Route-Helper-Credential", credential)
	req.Header.Set("X-Tclaude-Route-Agent-ID", agentID)
	req.Header.Set("X-Tclaude-Route-Conv-ID", convID)
	req.Header.Set("X-Tclaude-Route-Launch-Generation", generation)
	// Bound the authenticated status exchange so a stalled daemon surfaces here
	// rather than as a silent gap before the attach stage.
	_ = conn.SetDeadline(time.Now().Add(linuxRouteM6AttachTimeout))
	if err := req.Write(conn); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufioReader(conn), req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("endpoint status: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// bufioReader is kept as a tiny local constructor so the child status path
// cannot accidentally use an ordinary TCP client.
func bufioReader(conn net.Conn) *bufio.Reader { return bufio.NewReader(conn) }

func linuxRouteM6Write(t *testing.T, value string) {
	t.Helper()
	path := strings.TrimSpace(os.Getenv(linuxRouteM6ReadyEnv))
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	require.NoError(t, err)
	_, err = file.WriteString(value + "\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())
}

// linuxRouteM6Stage records that the child reached a named production boundary.
// The markers are what turn a stalled helper into an attributable stage.
func linuxRouteM6Stage(t *testing.T, stage string) {
	t.Helper()
	linuxRouteM6Write(t, "stage:"+stage)
}

// linuxRouteM6StageFailed publishes the failing stage before the child dies, so
// the host cell reports the real boundary instead of waiting out its deadline.
func linuxRouteM6StageFailed(t *testing.T, stage string, err error) {
	t.Helper()
	linuxRouteM6Write(t, "stage-failed:"+stage+": "+err.Error())
	t.Fatalf("Linux route smoke stage %s failed: %v", stage, err)
}

func linuxRouteM6Exists(path string) bool {
	_, err := os.Stat(strings.TrimSpace(path))
	return err == nil
}

func linuxRouteM6WaitStop(path string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		for !linuxRouteM6Exists(path) {
			time.Sleep(20 * time.Millisecond)
		}
		close(done)
	}()
	return done
}

func linuxRouteM6WaitFile(path string, timeout time.Duration) (string, error) {
	// An empty path is a hand-off bug, not a slow peer. Polling it would burn
	// the whole budget and then report an anonymous timeout, which is exactly
	// how TCL-960's root cause stayed hidden. Fail immediately and name it.
	if strings.TrimSpace(path) == "" {
		return "", errors.New("control-file path was not handed to the namespace child")
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(raw)) != "" {
			return string(raw), nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "", fmt.Errorf("timed out waiting for %s", path)
}

func TestLinuxRouteM6WaitFileRejectsMissingHandoff(t *testing.T) {
	start := time.Now()
	_, err := linuxRouteM6WaitFile("", time.Minute)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not handed to the namespace child")
	require.Less(t, time.Since(start), 5*time.Second, "a missing hand-off must fail promptly, not consume the wait budget")

	value, err := linuxRouteM6WaitFile(writeLinuxRouteM6Temp(t, "rte_x|3"), time.Second)
	require.NoError(t, err)
	require.Equal(t, "rte_x|3", strings.TrimSpace(value))

	_, err = linuxRouteM6WaitFile(filepath.Join(t.TempDir(), "absent"), 100*time.Millisecond)
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out waiting for")
}

func writeLinuxRouteM6Temp(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "descriptor")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

type linuxRouteM6Process struct {
	cmd    *exec.Cmd
	paths  linuxRouteM6ControlPaths
	output *bytes.Buffer
	done   chan struct{}
	mu     sync.Mutex
	err    error
}

func (p *linuxRouteM6Process) exited() (bool, error) {
	if p == nil {
		return true, nil
	}
	select {
	case <-p.done:
		p.mu.Lock()
		err := p.err
		p.mu.Unlock()
		return true, err
	default:
		return false, nil
	}
}

func (p *linuxRouteM6Process) wait() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *linuxRouteM6Process) stop(t *testing.T) {
	t.Helper()
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if exited, _ := p.exited(); !exited {
		_ = os.WriteFile(p.paths.Stop, []byte("stop"), 0o600)
	}
	if err := p.wait(); err != nil {
		t.Fatalf("Linux route smoke child exited: %v\n%s", err, p.output.String())
	}
}

func (p *linuxRouteM6Process) kill(t *testing.T) {
	t.Helper()
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if exited, _ := p.exited(); !exited {
		require.NoError(t, p.cmd.Process.Kill())
	}
	err := p.wait()
	require.True(t, linuxRouteM6ExpectedKilled(err), "intentional publisher kill must report SIGKILL, got %v", err)
}

func linuxRouteM6ExpectedKilled(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ProcessState == nil {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

func TestLinuxRouteM6ExpectedKilled(t *testing.T) {
	killed := exec.Command("sh", "-c", "kill -KILL $$").Run()
	require.True(t, linuxRouteM6ExpectedKilled(killed), "shell SIGKILL must be recognized: %v", killed)
	normalExit := exec.Command("sh", "-c", "exit 1").Run()
	require.False(t, linuxRouteM6ExpectedKilled(normalExit), "ordinary child failure must remain visible")
	require.False(t, linuxRouteM6ExpectedKilled(errors.New("unexpected child failure")))
}

func linuxRouteM6BubblewrapArgs(executable, control string) []string {
	return []string{
		"--die-with-parent", "--unshare-all", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
		"--tmpfs", "/tmp", "--tmpfs", "/run", "--dir", linuxRouteM6HelperDir, "--bind", control, control,
		"--ro-bind", executable, linuxRouteM6HelperPath,
		"--", linuxRouteM6HelperPath, "-test.run=^TestLinuxRouteM6Child$", "-test.count=1",
	}
}

func TestLinuxRouteM6BubblewrapArgsKeepHelperVisible(t *testing.T) {
	control := "/tmp/tcl952-route-control"
	executable := "/tmp/go-build/tcl952-agentd.test"
	args := linuxRouteM6BubblewrapArgs(executable, control)
	require.Contains(t, args, "--tmpfs")
	require.Contains(t, args, "/tmp")
	bindIndex := -1
	for i := 0; i+3 < len(args); i++ {
		if args[i] == "--ro-bind" && args[i+1] == executable {
			bindIndex = i
			break
		}
	}
	require.GreaterOrEqual(t, bindIndex, 0)
	require.Equal(t, linuxRouteM6HelperPath, args[bindIndex+2], "helper must be rebound outside the overlaid /tmp")
	runIndex := -1
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--tmpfs" && args[i+1] == "/run" {
			runIndex = i
			break
		}
	}
	require.GreaterOrEqual(t, runIndex, 0)
	dirIndex := -1
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--dir" && args[i+1] == linuxRouteM6HelperDir {
			dirIndex = i
			break
		}
	}
	require.GreaterOrEqual(t, dirIndex, 0)
	require.Less(t, runIndex, dirIndex, "private /run tmpfs must precede the helper directory")
	require.Less(t, dirIndex, bindIndex, "helper directory must exist before the read-only helper bind")
	commandIndex := -1
	for i, arg := range args {
		if arg == "--" {
			commandIndex = i + 1
			break
		}
	}
	require.GreaterOrEqual(t, commandIndex, 0)
	require.NotEqual(t, executable, args[commandIndex], "final exec must use the namespace-visible helper path")
	require.Equal(t, linuxRouteM6HelperPath, args[commandIndex])
	require.Equal(t, linuxRouteM6HelperPath, args[bindIndex+2])
	for i, arg := range args {
		if arg == "--tmpfs" {
			require.Contains(t, []string{"/tmp", "/run"}, args[i+1])
		}
	}
	// The control directory must be bound read-write: it carries the descriptor
	// and lease hand-off the child blocks on.
	controlIndex := -1
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--bind" && args[i+1] == control && args[i+2] == control {
			controlIndex = i
			break
		}
	}
	require.GreaterOrEqual(t, controlIndex, 0, "control directory must be bound read-write for the hand-off files")
}

func startLinuxRouteM6Process(t *testing.T, role, socketPath, control, credential, agentID, convID, generation string, hostPort int) *linuxRouteM6Process {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	paths := linuxRouteM6PathsFor(control, role)
	cmd := exec.Command("bwrap", linuxRouteM6BubblewrapArgs(executable, control)...)
	cmd.Env = linuxRouteM6ChildEnvironment(os.Environ(), role, socketPath, control, credential, agentID, convID, generation, hostPort)
	output := new(bytes.Buffer)
	cmd.Stdout = output
	cmd.Stderr = output
	require.NoError(t, cmd.Start())
	process := &linuxRouteM6Process{cmd: cmd, paths: paths, output: output, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		process.mu.Lock()
		process.err = err
		process.mu.Unlock()
		close(process.done)
	}()
	t.Cleanup(func() {
		if exited, _ := process.exited(); !exited {
			_ = os.WriteFile(paths.Stop, []byte("cleanup"), 0o600)
			_ = cmd.Process.Kill()
		}
		_ = process.wait()
	})
	return process
}

func serveLinuxRouteM6(t *testing.T, handler http.Handler, method, path, convID string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := AsAgentPeer(testharness.JSONRequest(t, method, path, body), convID)
	rec := testharness.Serve(handler, req)
	var out map[string]any
	if rec.Body.Len() != 0 {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	}
	return rec, out
}

func waitLinuxRouteM6Marker(t *testing.T, path, marker string, processes ...*linuxRouteM6Process) string {
	t.Helper()
	deadline := time.Now().Add(linuxRouteM6MarkerTimeout)
	var last string
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			last = string(raw)
			if strings.Contains(last, marker) {
				return last
			}
			// A published stage failure is terminal; report the named boundary
			// immediately instead of waiting out the deadline.
			if index := strings.Index(last, "stage-failed:"); index >= 0 {
				t.Fatalf("Linux route smoke child failed a production stage before marker %q: %s",
					marker, strings.TrimSpace(strings.Split(last[index:], "\n")[0]))
			}
		}
		for _, process := range processes {
			if exited, err := process.exited(); exited {
				t.Fatalf("Linux route smoke child exited before marker %q: %v\nchild markers:\n%s\nchild output:\n%s",
					marker, err, last, process.output.String())
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %s; last output %q", marker, path, last)
	return last
}

// linuxRouteM6GroupGeneration reads the authoritative group generation out of a
// route or lease API response.
//
// The helper must present exactly the generation its own route or lease row
// carries, because that is what the production authority compares. Every
// membership and permission mutation advances a group's route generation, so a
// value snapshotted from the group before those mutations is refused by
// POST /v1/routes/channel as a stale publisher identity — a refusal about the
// fixture's bookkeeping rather than the boundary under test. Taking it from the
// response also matches how a real helper learns its route identity.
func linuxRouteM6GroupGeneration(t *testing.T, view map[string]any) int64 {
	t.Helper()
	raw, ok := view["group_generation"].(float64)
	require.True(t, ok, "route API response must expose group_generation: %v", view)
	generation := int64(raw)
	require.Positive(t, generation, "group generation must be positive: %v", view)
	return generation
}

// TestLinuxRouteChannelPinsCurrentGroupGeneration pins the exact generation
// identity POST /v1/routes/channel admits. It runs in ordinary CI without
// Bubblewrap and covers both directions: the generation a publish/open response
// hands back is accepted, and a generation snapshotted before a group mutation
// is refused. Regression guard for the second TCL-960 boundary, where the smoke
// presented a pre-permission-grant snapshot and was correctly refused 403
// route_authority.
func TestLinuxRouteChannelPinsCurrentGroupGeneration(t *testing.T) {
	setupTestDB(t)
	const publisherConv = "tcl960-generation-publisher"
	const consumerConv = "tcl960-generation-consumer"
	publisherAgent, _, err := db.EnsureAgentForConv(publisherConv, "TCL-960 generation pin")
	require.NoError(t, err)
	consumerAgent, _, err := db.EnsureAgentForConv(consumerConv, "TCL-960 generation pin")
	require.NoError(t, err)
	groupID, err := db.CreateAgentGroup("tcl960-generation-pin", "generation pin")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: publisherConv}))
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: consumerConv}))

	// Snapshot the group exactly as a fixture that captures its generation too
	// early would, then perform an ordinary mutation that advances it.
	snapshot, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(groupID, []string{PermRoutesPublish, PermRoutesConsume}, "TCL-960"))
	group, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)
	require.Greater(t, group.RouteGeneration, snapshot.RouteGeneration,
		"granting route permissions must advance the group route generation")

	publisherCredential, publisherGeneration, err := mintRouteHelperCredential(publisherAgent, publisherConv)
	require.NoError(t, err)
	consumerCredential, consumerGeneration, err := mintRouteHelperCredential(consumerAgent, consumerConv)
	require.NoError(t, err)
	require.NotEmpty(t, publisherCredential)
	require.NotEmpty(t, consumerCredential)
	t.Cleanup(func() {
		revokeRouteHelperCredentials(publisherConv, "")
		revokeRouteHelperCredentials(consumerConv, "")
	})
	saveLinuxRouteM6Session(t, publisherConv, publisherGeneration)
	saveLinuxRouteM6Session(t, consumerConv, consumerGeneration)

	target, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer target.Close()

	handler := BuildHandlerForTest()
	rec, route := serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/routes/publish", publisherConv, map[string]any{
		"group": group.Name, "name": "pin", "target": "tcp://" + target.Addr().String(), "launch_generation": publisherGeneration,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	routeID := route["id"].(string)
	publishedGeneration := linuxRouteM6GroupGeneration(t, route)
	require.Equal(t, group.RouteGeneration, publishedGeneration,
		"a published route must carry the group's current route generation")
	require.NotEqual(t, snapshot.RouteGeneration, publishedGeneration,
		"the pre-mutation snapshot must not be usable as the route's generation")

	// AuthorizePublisher is the exact check POST /v1/routes/channel runs before
	// it will hijack the connection and admit a data channel.
	publisherAuth := routebroker.PublisherAuth{
		RouteID: routeID, AgentID: publisherAgent, ConvID: publisherConv,
		LaunchGeneration: publisherGeneration, GroupGeneration: publishedGeneration,
	}
	require.NoError(t, databaseRouteAuthority{}.AuthorizePublisher(context.Background(), publisherAuth),
		"the generation the publish response returned must be admitted")
	stalePublisher := publisherAuth
	stalePublisher.GroupGeneration = snapshot.RouteGeneration
	require.Error(t, databaseRouteAuthority{}.AuthorizePublisher(context.Background(), stalePublisher),
		"a generation snapshotted before a group mutation must stay refused")

	rec, lease := serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/routes/open", consumerConv, map[string]any{
		"route_id": routeID, "group": group.Name, "launch_generation": consumerGeneration,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	leaseGeneration := linuxRouteM6GroupGeneration(t, lease)
	require.Equal(t, publishedGeneration, leaseGeneration,
		"a lease must carry the same current group generation as its route")
	consumerAuth := routebroker.ConsumerAuth{
		LeaseID: lease["id"].(string), RouteID: routeID, AgentID: consumerAgent, ConvID: consumerConv,
		LaunchGeneration: consumerGeneration, GroupGeneration: leaseGeneration,
	}
	require.NoError(t, databaseRouteAuthority{}.AuthorizeConsumer(context.Background(), consumerAuth),
		"the generation the open response returned must be admitted")
	staleConsumer := consumerAuth
	staleConsumer.GroupGeneration = snapshot.RouteGeneration
	require.Error(t, databaseRouteAuthority{}.AuthorizeConsumer(context.Background(), staleConsumer),
		"a consumer generation snapshotted before a group mutation must stay refused")
}

func saveLinuxRouteM6Session(t *testing.T, convID, generation string) string {
	t.Helper()
	sessionID := "tcl952-linux-route-" + strings.ReplaceAll(convID, "-", "")
	now := time.Now()
	require.NoError(t, db.SaveSession(&db.SessionRow{ID: sessionID, ConvID: convID, Cwd: "/tmp", Status: "working", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, db.SetSessionExitLaunchGeneration(sessionID, generation))
	return sessionID
}

func TestLinuxRouteCapabilityIntegratedSmoke(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv(linuxRouteM6EnabledEnv) != "1" {
		t.Skip("set TCLAUDE_LINUX_ROUTE_CAPABILITY_SMOKE=1 on the dedicated Linux evidence workflow")
	}
	_, err := exec.LookPath("bwrap")
	require.NoError(t, err, "authoritative Linux route evidence requires Bubblewrap")
	head, err := exec.Command("git", "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	actualHead := strings.TrimSpace(string(head))
	require.Len(t, actualHead, 40)
	if expectedHead := strings.TrimSpace(os.Getenv("EXPECTED_HEAD")); expectedHead != "" {
		require.Equal(t, expectedHead, actualHead, "dedicated evidence must run the requested exact checkout")
	}
	t.Logf("TCL-952 Linux exact checked-out head: %s", actualHead)

	setupTestDB(t)
	// Unix-domain paths are capped at 108 bytes on Linux. setupTestDB uses a
	// deliberately descriptive temporary HOME, so keep the bound control path
	// short while still making it process-unique for parallel test invocations.
	control := filepath.Join(os.TempDir(), fmt.Sprintf("tcl952-route-%d", os.Getpid()))
	require.NoError(t, os.MkdirAll(control, 0o700))
	t.Cleanup(func() { _ = os.RemoveAll(control) })
	socketPath := filepath.Join(control, "agentd.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	server := &http.Server{Handler: withIdentity(buildMux()), ReadHeaderTimeout: time.Second, ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
		if unixConn, ok := conn.(*net.UnixConn); ok {
			return context.WithValue(ctx, unixConnKey{}, unixConn)
		}
		return ctx
	}}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = listener.Close()
		<-serverDone
	})

	const publisherConv = "tcl952-linux-publisher"
	const consumerConv = "tcl952-linux-consumer"
	const controlConv = "tcl952-linux-control"
	publisherAgent, _, err := db.EnsureAgentForConv(publisherConv, "TCL-952 Linux route smoke")
	require.NoError(t, err)
	consumerAgent, _, err := db.EnsureAgentForConv(consumerConv, "TCL-952 Linux route smoke")
	require.NoError(t, err)
	_, _, err = db.EnsureAgentForConv(controlConv, "TCL-952 Linux route smoke")
	require.NoError(t, err)
	groupID, err := db.CreateAgentGroup("tcl952-linux-route-group", "authoritative Linux route smoke")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: publisherConv}))
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: consumerConv}))
	group, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(group.ID, []string{PermRoutesPublish, PermRoutesConsume}, "TCL-952"))
	wrongGroupID, err := db.CreateAgentGroup("tcl952-linux-control-group", "different-group control")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: wrongGroupID, ConvID: controlConv}))
	require.NoError(t, db.ReplaceAgentGroupPermissions(wrongGroupID, []string{PermRoutesConsume}, "TCL-952"))

	publisherCredential, publisherGeneration, err := mintRouteHelperCredential(publisherAgent, publisherConv)
	require.NoError(t, err)
	consumerCredential, consumerGeneration, err := mintRouteHelperCredential(consumerAgent, consumerConv)
	require.NoError(t, err)
	t.Cleanup(func() {
		revokeRouteHelperCredentials(publisherConv, "")
		revokeRouteHelperCredentials(consumerConv, "")
	})
	saveLinuxRouteM6Session(t, publisherConv, publisherGeneration)
	saveLinuxRouteM6Session(t, consumerConv, consumerGeneration)
	hostControl, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer hostControl.Close()
	hostPort := hostControl.Addr().(*net.TCPAddr).Port

	publisherPaths := linuxRouteM6PathsFor(control, "publisher")
	consumerPaths := linuxRouteM6PathsFor(control, "consumer")

	pub := startLinuxRouteM6Process(t, "publisher", socketPath, control, publisherCredential, publisherAgent, publisherConv, publisherGeneration, hostPort)
	pubTarget := waitLinuxRouteM6Marker(t, publisherPaths.Ready, "target=", pub)
	targetLine := pubTarget[strings.LastIndex(pubTarget, "target=")+len("target="):]
	targetLine = strings.TrimSpace(strings.Split(targetLine, "\n")[0])

	handler := BuildHandlerForTest()
	rec, route := serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/routes/publish", publisherConv, map[string]any{
		"group": group.Name, "name": "api", "target": targetLine, "launch_generation": publisherGeneration,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	routeID := route["id"].(string)
	require.Equal(t, "ready", route["state"])
	routeGroupGeneration := linuxRouteM6GroupGeneration(t, route)
	require.NoError(t, os.WriteFile(publisherPaths.Descriptor, []byte(routeID+"|"+strconv.FormatInt(routeGroupGeneration, 10)), 0o600))
	pubEvidence := waitLinuxRouteM6Marker(t, publisherPaths.Ready, "policy-floor:host-and-internet-denied", pub)
	t.Logf("TCL-952 Linux publisher policy floor: %s", pubEvidence)
	pubStages := waitLinuxRouteM6Marker(t, publisherPaths.Ready, "channel-attached", pub)
	t.Logf("TCL-952 Linux publisher stage evidence: %s", strings.Join(linuxRouteM6Stages(pubStages), " -> "))
	require.Eventually(t, func() bool { return GroupRouteBroker().Metrics().PublisherChannels == 1 }, time.Second, 10*time.Millisecond)

	// Negative evidence is intentionally adjacent to the positive route: an
	// unpublished neighbor, a stale launch, a different group, and an arbitrary
	// host target all fail before a data channel is admitted.
	rec, body := serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/routes/open", consumerConv, map[string]any{
		"route_id": "rte_unpublished-neighbor", "group": group.Name,
	})
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Equal(t, "route_not_found", body["code"])
	staleGroupGeneration := routeGroupGeneration - 1
	rec, body = serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/routes/publish", publisherConv, map[string]any{
		"group": group.Name, "group_generation": staleGroupGeneration, "name": "stale-group", "target": targetLine, "launch_generation": publisherGeneration,
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Equal(t, "route_conflict", body["code"])
	rec, body = serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/routes/publish", publisherConv, map[string]any{
		"group": group.Name, "name": "stale", "target": targetLine, "launch_generation": "stale-generation",
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Equal(t, "route_generation_stale", body["code"])
	rec, body = serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/routes/open", controlConv, map[string]any{
		"route_id": routeID, "group": group.Name,
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Equal(t, "route_not_member", body["code"])
	rec, body = serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/routes/publish", publisherConv, map[string]any{
		"group": group.Name, "name": "host", "target": "tcp://192.0.2.1:443", "launch_generation": publisherGeneration,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, "route_target_not_local", body["code"])
	t.Log("TCL-952 Linux negative evidence: unpublished neighbor, wrong group, stale group/launch generation, and host target denied")

	consumer := startLinuxRouteM6Process(t, "consumer", socketPath, control, consumerCredential, consumerAgent, consumerConv, consumerGeneration, hostPort)
	consumerEndpointLine := waitLinuxRouteM6Marker(t, consumerPaths.Ready, "listener=", consumer)
	endpoint := strings.TrimSpace(strings.Split(consumerEndpointLine[strings.LastIndex(consumerEndpointLine, "listener=")+len("listener="):], "\n")[0])
	rec, lease := serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/routes/open", consumerConv, map[string]any{
		"route_id": routeID, "group": group.Name, "launch_generation": consumerGeneration,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	leaseID := lease["id"].(string)
	require.Equal(t, "pending", lease["endpoint_state"])
	require.NoError(t, os.WriteFile(consumerPaths.Lease, []byte(leaseID+"|"+routeID+"|"+strconv.FormatInt(linuxRouteM6GroupGeneration(t, lease), 10)), 0o600))
	_ = endpoint // The child reports the endpoint through the authenticated status callback.
	consumerStages := waitLinuxRouteM6Marker(t, consumerPaths.Ready, "channel-attached", consumer)
	t.Logf("TCL-952 Linux consumer stage evidence: %s", strings.Join(linuxRouteM6Stages(consumerStages), " -> "))

	ordinaryAccepted := 0
	ordinaryObserved := 0
	for i := 0; i < linuxRouteM6Count; i++ {
		messageBody := fmt.Sprintf("route-smoke-message-%d", i)
		rec, sent := serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/messages", publisherConv, map[string]any{
			"to": consumerConv, "body": messageBody,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		ordinaryAccepted++
		messageID, ok := sent["id"].(float64)
		require.True(t, ok, "ordinary message response must expose its id: %s", rec.Body.String())
		rec, _ = serveLinuxRouteM6(t, handler, http.MethodGet, "/v1/messages/"+strconv.FormatInt(int64(messageID), 10), consumerConv, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), messageBody)
		ordinaryObserved++
	}
	waitLinuxRouteM6Marker(t, consumerPaths.Ready, fmt.Sprintf("sustained-route-traffic:%d", linuxRouteM6Count), consumer)
	require.Equal(t, linuxRouteM6Count, ordinaryAccepted, "all ordinary messages must be accepted")
	require.Equal(t, ordinaryAccepted, ordinaryObserved, "all accepted ordinary messages must be observed through the recipient read path")
	t.Logf("TCL-952 Linux sustained route evidence: ordinary messaging accepted=%d observed=%d while opaque traffic continued", ordinaryAccepted, ordinaryObserved)

	rec, current := serveLinuxRouteM6(t, handler, http.MethodGet, "/v1/routes/"+routeID, consumerConv, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, db.RouteStateReady, current["state"])
	t.Logf("TCL-952 Linux launch disclosure: route capability current; route=%s group-generation=%d", routeID, routeGroupGeneration)
	rec, _ = serveLinuxRouteM6(t, handler, http.MethodDelete, "/v1/routes/"+routeID, publisherConv, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	// Both channel closures must be caused by the withdrawal, so neither stop
	// file may be written until both have been observed. Writing a stop file
	// first would close the consumer itself and let the cell pass on evidence it
	// manufactured rather than on what withdrawal actually tore down.
	waitLinuxRouteM6Marker(t, publisherPaths.Ready, "channel-closed", pub)
	waitLinuxRouteM6Marker(t, consumerPaths.Ready, "channel-closed", consumer)
	// The durable records behind those closures are asserted through the
	// production read paths, so the cell cannot pass on socket behaviour alone.
	rec, withdrawn := serveLinuxRouteM6(t, handler, http.MethodGet, "/v1/routes/"+routeID, consumerConv, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, db.RouteStateWithdrawn, withdrawn["state"], "explicit withdrawal must be visible through the route read path")
	withdrawnLease, err := db.GetAgentRouteLease(leaseID)
	require.NoError(t, err)
	require.Equal(t, db.RouteLeaseClosed, withdrawnLease.State, "withdrawal must close the consumer lease")
	require.Eventually(t, func() bool { return GroupRouteBroker().Metrics().PublisherChannels == 0 }, 5*time.Second, 10*time.Millisecond,
		"withdrawal must detach the publisher channel")
	require.NoError(t, os.WriteFile(publisherPaths.Stop, []byte("stop"), 0o600))
	require.NoError(t, os.WriteFile(consumerPaths.Stop, []byte("stop"), 0o600))
	pub.stop(t)
	consumer.stop(t)
	t.Log("TCL-952 Linux lifecycle evidence: publisher withdrawal closed both attached channels; route withdrawn and lease closed through the production read path")

	// A second route is kept solely to attribute publisher-exit withdrawal to
	// the lifecycle event rather than the explicit DELETE above.
	const exitPublisherConv = "tcl952-linux-exit-publisher"
	exitPublisherAgent, _, err := db.EnsureAgentForConv(exitPublisherConv, "TCL-952 Linux route smoke")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: exitPublisherConv}))
	exitCredential, exitGeneration, err := mintRouteHelperCredential(exitPublisherAgent, exitPublisherConv)
	require.NoError(t, err)
	exitSessionID := saveLinuxRouteM6Session(t, exitPublisherConv, exitGeneration)
	// The first publisher used role-scoped control files. Remove its terminal
	// markers before starting the second child so the exit-withdrawal cell
	// cannot consume stale route or stop state.
	require.NoError(t, os.Remove(publisherPaths.Stop))
	require.NoError(t, os.Remove(publisherPaths.Descriptor))
	require.NoError(t, os.Remove(publisherPaths.Ready))
	exitPub := startLinuxRouteM6Process(t, "publisher", socketPath, control, exitCredential, exitPublisherAgent, exitPublisherConv, exitGeneration, hostPort)
	exitTargetOutput := waitLinuxRouteM6Marker(t, publisherPaths.Ready, "target=", exitPub)
	exitTarget := strings.TrimSpace(strings.Split(exitTargetOutput[strings.LastIndex(exitTargetOutput, "target=")+len("target="):], "\n")[0])
	rec, exitRoute := serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/routes/publish", exitPublisherConv, map[string]any{
		"group": group.Name, "name": "publisher-exit", "target": exitTarget, "launch_generation": exitGeneration,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	exitRouteID := exitRoute["id"].(string)
	require.NoError(t, os.WriteFile(publisherPaths.Descriptor, []byte(exitRouteID+"|"+strconv.FormatInt(linuxRouteM6GroupGeneration(t, exitRoute), 10)), 0o600))
	waitLinuxRouteM6Marker(t, publisherPaths.Ready, "channel-attached", exitPub)
	require.NoError(t, os.Remove(consumerPaths.Stop))
	require.NoError(t, os.Remove(consumerPaths.Lease))
	require.NoError(t, os.Remove(consumerPaths.Ready))
	exitConsumer := startLinuxRouteM6Process(t, "consumer", socketPath, control, consumerCredential, consumerAgent, consumerConv, consumerGeneration, hostPort)
	exitConsumerEndpointOutput := waitLinuxRouteM6Marker(t, consumerPaths.Ready, "listener=", exitConsumer)
	exitConsumerEndpoint := strings.TrimSpace(strings.Split(exitConsumerEndpointOutput[strings.LastIndex(exitConsumerEndpointOutput, "listener=")+len("listener="):], "\n")[0])
	rec, exitLease := serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/routes/open", consumerConv, map[string]any{
		"route_id": exitRouteID, "group": group.Name, "launch_generation": consumerGeneration,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	exitLeaseID := exitLease["id"].(string)
	require.NoError(t, os.WriteFile(consumerPaths.Lease, []byte(exitLeaseID+"|"+exitRouteID+"|"+strconv.FormatInt(linuxRouteM6GroupGeneration(t, exitLease), 10)), 0o600))
	waitLinuxRouteM6Marker(t, consumerPaths.Ready, "channel-attached", exitConsumer)
	// This cell is about what a publisher *exit* tears down, so the consumer
	// must be finished with the route before the publisher is killed. Attaching
	// is not enough: the consumer opens its short-lived connections as soon as
	// it attaches, and killing the publisher underneath that loop fails an
	// in-flight exchange on a teardown the cell deliberately caused.
	waitLinuxRouteM6Marker(t, consumerPaths.Ready, fmt.Sprintf("sustained-route-traffic:%d", linuxRouteM6Count), exitConsumer, exitPub)
	exitPub.kill(t)
	observedExitSession, err := db.LoadSession(exitSessionID)
	require.NoError(t, err)
	require.Equal(t, "working", observedExitSession.Status)
	exitAccepted, _, err := db.MarkSessionExitedAndRecordObservationIfUnchanged(
		exitSessionID, observedExitSession.Status, observedExitSession.UpdatedAt, "unexpected",
		db.AgentExitObservation{
			At: time.Now(), SessionID: exitSessionID, Observer: db.AgentExitObserverReaper,
			CauseKind: db.AgentExitCauseDisappeared, ObservedState: "exited", ExpectedGeneration: exitGeneration,
		},
	)
	require.NoError(t, err)
	require.True(t, exitAccepted, "generation-bound production reaper transaction must accept the publisher exit")
	revokeRouteHelperCredentials(exitPublisherConv, exitGeneration)
	rec, exitView := serveLinuxRouteM6(t, handler, http.MethodGet, "/v1/routes/"+exitRouteID, consumerConv, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, db.RouteStatePublisherLost, exitView["state"])
	exitLeaseRow, err := db.GetAgentRouteLease(exitLeaseID)
	require.NoError(t, err)
	require.Equal(t, db.RouteLeaseClosed, exitLeaseRow.State)
	waitLinuxRouteM6Marker(t, consumerPaths.Ready, "channel-closed", exitConsumer)
	require.Eventually(t, func() bool { return GroupRouteBroker().Metrics().PublisherChannels == 0 }, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		conn, dialErr := net.DialTimeout("tcp4", strings.TrimPrefix(exitConsumerEndpoint, "tcp://"), 250*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return false
		}
		return true
	}, 5*time.Second, 50*time.Millisecond, "publisher exit must close the consumer endpoint")
	t.Log("TCL-952 Linux publisher-exit evidence: generation-bound reaper transaction withdrew route, closed lease/channel/endpoint")

	t.Log("TCL-952 Linux evidence: POSITIVE production routeadapter/bwrap opaque TCP activation")
}

// linuxRouteM6Stages extracts the ordered production boundaries a child
// reported, so the evidence log names what was crossed rather than only that
// the run ended well.
func linuxRouteM6Stages(markers string) []string {
	var stages []string
	for _, line := range strings.Split(markers, "\n") {
		if stage, ok := strings.CutPrefix(strings.TrimSpace(line), "stage:"); ok {
			stages = append(stages, stage)
		}
	}
	return stages
}

func TestLinuxRouteM6StagesReportsOrderedBoundaries(t *testing.T) {
	markers := "target=tcp://127.0.0.1:1\nstage:descriptor-wait\npolicy-floor:host-and-internet-denied\nstage:descriptor-ok:rte_x\nstage:channel-dial\nchannel-attached\n"
	require.Equal(t, []string{"descriptor-wait", "descriptor-ok:rte_x", "channel-dial"}, linuxRouteM6Stages(markers))
	require.Empty(t, linuxRouteM6Stages("target=tcp://127.0.0.1:1\nchannel-attached\n"))
}
