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
	linuxRouteM6GroupEnv   = "TCL952_LINUX_ROUTE_GROUP_GENERATION"
	linuxRouteM6RouteEnv   = "TCL952_LINUX_ROUTE_DESCRIPTOR"
	linuxRouteM6LeaseEnv   = "TCL952_LINUX_ROUTE_LEASE"
	linuxRouteM6HostEnv    = "TCL952_LINUX_ROUTE_HOST_PORT"
	linuxRouteM6Opaque     = "tcl952-linux-opaque"
	linuxRouteM6Count      = 96
	linuxRouteM6HelperDir  = "/run/tcl952-route"
	linuxRouteM6HelperPath = linuxRouteM6HelperDir + "/helper"
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

	descriptor, err := linuxRouteM6WaitFile(os.Getenv(linuxRouteM6RouteEnv), 30*time.Second)
	require.NoError(t, err)
	routeID, groupGeneration, err := parseLinuxRouteM6PublisherDescriptor(descriptor)
	require.NoError(t, err)
	channel, err := routeadapter.DialUnixChannel(context.Background(), os.Getenv(linuxRouteM6SocketEnv), routeadapter.ChannelAuth{
		Role:             routeadapter.RolePublisher,
		RouteID:          routeID,
		AgentID:          os.Getenv(linuxRouteM6AgentEnv),
		ConvID:           os.Getenv(linuxRouteM6ConvEnv),
		LaunchGeneration: os.Getenv(linuxRouteM6GenEnv),
		GroupGeneration:  groupGeneration,
		Credential:       os.Getenv(linuxRouteM6CredEnv),
	})
	require.NoError(t, err)
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
	descriptor, err := linuxRouteM6WaitFile(os.Getenv(linuxRouteM6LeaseEnv), 30*time.Second)
	require.NoError(t, err)
	parts := strings.Split(strings.TrimSpace(descriptor), "|")
	require.Len(t, parts, 3)
	groupGeneration, err := strconv.ParseInt(parts[2], 10, 64)
	require.NoError(t, err)
	require.NoError(t, linuxRouteM6SetEndpointStatus(
		os.Getenv(linuxRouteM6SocketEnv), parts[0], endpoint,
		os.Getenv(linuxRouteM6CredEnv), os.Getenv(linuxRouteM6AgentEnv), os.Getenv(linuxRouteM6ConvEnv), os.Getenv(linuxRouteM6GenEnv)))
	channel, err := routeadapter.DialUnixChannel(context.Background(), os.Getenv(linuxRouteM6SocketEnv), routeadapter.ChannelAuth{
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
	require.NoError(t, err)
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
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(raw)) != "" {
			return string(raw), nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "", fmt.Errorf("timed out waiting for %s", path)
}

type linuxRouteM6Process struct {
	cmd     *exec.Cmd
	control string
	output  *bytes.Buffer
	done    chan struct{}
	mu      sync.Mutex
	err     error
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
		_ = os.WriteFile(filepath.Join(p.control, "stop"), []byte("stop"), 0o600)
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
}

func startLinuxRouteM6Process(t *testing.T, role, socketPath, control, credential, agentID, convID, generation string, hostPort int) *linuxRouteM6Process {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	ready := filepath.Join(control, role+".ready")
	stop := filepath.Join(control, role+".stop")
	cmd := exec.Command("bwrap", linuxRouteM6BubblewrapArgs(executable, control)...)
	cmd.Env = append(os.Environ(),
		linuxRouteM6ChildEnv+"=1",
		linuxRouteM6RoleEnv+"="+role,
		linuxRouteM6SocketEnv+"="+socketPath,
		linuxRouteM6ControlEnv+"="+control,
		linuxRouteM6ReadyEnv+"="+ready,
		linuxRouteM6StopEnv+"="+stop,
		linuxRouteM6CredEnv+"="+credential,
		linuxRouteM6AgentEnv+"="+agentID,
		linuxRouteM6ConvEnv+"="+convID,
		linuxRouteM6GenEnv+"="+generation,
		linuxRouteM6HostEnv+"="+strconv.Itoa(hostPort),
	)
	output := new(bytes.Buffer)
	cmd.Stdout = output
	cmd.Stderr = output
	require.NoError(t, cmd.Start())
	process := &linuxRouteM6Process{cmd: cmd, control: control, output: output, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		process.mu.Lock()
		process.err = err
		process.mu.Unlock()
		close(process.done)
	}()
	t.Cleanup(func() {
		if exited, _ := process.exited(); !exited {
			_ = os.WriteFile(stop, []byte("cleanup"), 0o600)
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
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			last = string(raw)
			if strings.Contains(last, marker) {
				return last
			}
		}
		for _, process := range processes {
			if exited, err := process.exited(); exited {
				t.Fatalf("Linux route smoke child exited before marker %q: %v\n%s", marker, err, process.output.String())
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %s; last output %q", marker, path, last)
	return last
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

	pub := startLinuxRouteM6Process(t, "publisher", socketPath, control, publisherCredential, publisherAgent, publisherConv, publisherGeneration, hostPort)
	pubReadyPath := filepath.Join(control, "publisher.ready")
	pubTarget := waitLinuxRouteM6Marker(t, pubReadyPath, "target=", pub)
	targetLine := pubTarget[strings.LastIndex(pubTarget, "target=")+len("target="):]
	targetLine = strings.TrimSpace(strings.Split(targetLine, "\n")[0])

	handler := BuildHandlerForTest()
	rec, route := serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/routes/publish", publisherConv, map[string]any{
		"group": group.Name, "name": "api", "target": targetLine, "launch_generation": publisherGeneration,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	routeID := route["id"].(string)
	require.Equal(t, "ready", route["state"])
	require.NoError(t, os.WriteFile(filepath.Join(control, "publisher.route"), []byte(routeID+"|"+strconv.FormatInt(group.RouteGeneration, 10)), 0o600))
	pubEvidence := waitLinuxRouteM6Marker(t, pubReadyPath, "policy-floor:host-and-internet-denied", pub)
	t.Logf("TCL-952 Linux publisher policy floor: %s", pubEvidence)
	waitLinuxRouteM6Marker(t, pubReadyPath, "channel-attached", pub)
	require.Eventually(t, func() bool { return GroupRouteBroker().Metrics().PublisherChannels == 1 }, time.Second, 10*time.Millisecond)

	// Negative evidence is intentionally adjacent to the positive route: an
	// unpublished neighbor, a stale launch, a different group, and an arbitrary
	// host target all fail before a data channel is admitted.
	rec, body := serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/routes/open", consumerConv, map[string]any{
		"route_id": "rte_unpublished-neighbor", "group": group.Name,
	})
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Equal(t, "route_not_found", body["code"])
	staleGroupGeneration := group.RouteGeneration - 1
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
	consumerReadyPath := filepath.Join(control, "consumer.ready")
	consumerEndpointLine := waitLinuxRouteM6Marker(t, consumerReadyPath, "listener=", consumer)
	endpoint := strings.TrimSpace(strings.Split(consumerEndpointLine[strings.LastIndex(consumerEndpointLine, "listener=")+len("listener="):], "\n")[0])
	rec, lease := serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/routes/open", consumerConv, map[string]any{
		"route_id": routeID, "group": group.Name, "launch_generation": consumerGeneration,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	leaseID := lease["id"].(string)
	require.Equal(t, "pending", lease["endpoint_state"])
	require.NoError(t, os.WriteFile(filepath.Join(control, "consumer.lease"), []byte(leaseID+"|"+routeID+"|"+strconv.FormatInt(group.RouteGeneration, 10)), 0o600))
	_ = endpoint // The child reports the endpoint through the authenticated status callback.
	waitLinuxRouteM6Marker(t, consumerReadyPath, "channel-attached", consumer)

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
	waitLinuxRouteM6Marker(t, consumerReadyPath, fmt.Sprintf("sustained-route-traffic:%d", linuxRouteM6Count), consumer)
	require.Equal(t, linuxRouteM6Count, ordinaryAccepted, "all ordinary messages must be accepted")
	require.Equal(t, ordinaryAccepted, ordinaryObserved, "all accepted ordinary messages must be observed through the recipient read path")
	t.Logf("TCL-952 Linux sustained route evidence: ordinary messaging accepted=%d observed=%d while opaque traffic continued", ordinaryAccepted, ordinaryObserved)

	rec, current := serveLinuxRouteM6(t, handler, http.MethodGet, "/v1/routes/"+routeID, consumerConv, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, db.RouteStateReady, current["state"])
	t.Logf("TCL-952 Linux launch disclosure: route capability current; route=%s group-generation=%d", routeID, group.RouteGeneration)
	rec, _ = serveLinuxRouteM6(t, handler, http.MethodDelete, "/v1/routes/"+routeID, publisherConv, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	waitLinuxRouteM6Marker(t, pubReadyPath, "channel-closed", pub)
	require.NoError(t, os.WriteFile(filepath.Join(control, "publisher.stop"), []byte("stop"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(control, "consumer.stop"), []byte("stop"), 0o600))
	pub.stop(t)
	consumer.stop(t)
	t.Log("TCL-952 Linux lifecycle evidence: publisher withdrawal closed attached channels")

	// A second route is kept solely to attribute publisher-exit withdrawal to
	// the lifecycle event rather than the explicit DELETE above.
	const exitPublisherConv = "tcl952-linux-exit-publisher"
	exitPublisherAgent, _, err := db.EnsureAgentForConv(exitPublisherConv, "TCL-952 Linux route smoke")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: exitPublisherConv}))
	exitCredential, exitGeneration, err := mintRouteHelperCredential(exitPublisherAgent, exitPublisherConv)
	require.NoError(t, err)
	exitSessionID := saveLinuxRouteM6Session(t, exitPublisherConv, exitGeneration)
	// The first publisher used role-scoped marker files. Remove its terminal
	// markers before starting the second child so the exit-withdrawal cell
	// cannot consume stale route or stop state.
	require.NoError(t, os.Remove(filepath.Join(control, "publisher.stop")))
	require.NoError(t, os.Remove(filepath.Join(control, "publisher.route")))
	require.NoError(t, os.Remove(filepath.Join(control, "publisher.ready")))
	exitPub := startLinuxRouteM6Process(t, "publisher", socketPath, control, exitCredential, exitPublisherAgent, exitPublisherConv, exitGeneration, hostPort)
	exitReadyPath := filepath.Join(control, "publisher.ready")
	exitTargetOutput := waitLinuxRouteM6Marker(t, exitReadyPath, "target=", exitPub)
	exitTarget := strings.TrimSpace(strings.Split(exitTargetOutput[strings.LastIndex(exitTargetOutput, "target=")+len("target="):], "\n")[0])
	rec, exitRoute := serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/routes/publish", exitPublisherConv, map[string]any{
		"group": group.Name, "name": "publisher-exit", "target": exitTarget, "launch_generation": exitGeneration,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	exitRouteID := exitRoute["id"].(string)
	require.NoError(t, os.WriteFile(filepath.Join(control, "publisher.route"), []byte(exitRouteID+"|"+strconv.FormatInt(group.RouteGeneration, 10)), 0o600))
	waitLinuxRouteM6Marker(t, exitReadyPath, "channel-attached", exitPub)
	require.NoError(t, os.Remove(filepath.Join(control, "consumer.stop")))
	require.NoError(t, os.Remove(filepath.Join(control, "consumer.lease")))
	require.NoError(t, os.Remove(filepath.Join(control, "consumer.ready")))
	exitConsumer := startLinuxRouteM6Process(t, "consumer", socketPath, control, consumerCredential, consumerAgent, consumerConv, consumerGeneration, hostPort)
	exitConsumerReadyPath := filepath.Join(control, "consumer.ready")
	exitConsumerEndpointOutput := waitLinuxRouteM6Marker(t, exitConsumerReadyPath, "listener=", exitConsumer)
	exitConsumerEndpoint := strings.TrimSpace(strings.Split(exitConsumerEndpointOutput[strings.LastIndex(exitConsumerEndpointOutput, "listener=")+len("listener="):], "\n")[0])
	rec, exitLease := serveLinuxRouteM6(t, handler, http.MethodPost, "/v1/routes/open", consumerConv, map[string]any{
		"route_id": exitRouteID, "group": group.Name, "launch_generation": consumerGeneration,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	exitLeaseID := exitLease["id"].(string)
	require.NoError(t, os.WriteFile(filepath.Join(control, "consumer.lease"), []byte(exitLeaseID+"|"+exitRouteID+"|"+strconv.FormatInt(group.RouteGeneration, 10)), 0o600))
	waitLinuxRouteM6Marker(t, exitConsumerReadyPath, "channel-attached", exitConsumer)
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
	waitLinuxRouteM6Marker(t, exitConsumerReadyPath, "channel-closed", exitConsumer)
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
