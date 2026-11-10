//go:build linux

package netnsdial

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const serveEnv = "NETNSDIAL_TEST_SERVE"

// requireEnv turns the environment skips into failures. CI sets it in the
// job that unlocks unprivileged user namespaces, so the live nsenter round
// trip cannot silently skip there.
const requireEnv = "TCLAUDE_REQUIRE_USERNS"

func skipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv(requireEnv) == "1" {
		t.Fatalf(requireEnv+"=1 but: "+format, args...)
	}
	t.Skipf(format, args...)
}

func TestMain(m *testing.M) {
	if handled, code := RunHelper(os.Args); handled {
		os.Exit(code)
	}
	if port := os.Getenv(serveEnv); port != "" {
		os.Exit(serveEcho(port))
	}
	os.Exit(m.Run())
}

// serveEcho runs inside the test's private namespace: it answers every
// connection with "inside\n".
func serveEcho(port string) int {
	ln, err := net.Listen("tcp4", "127.0.0.1:"+port)
	if err != nil {
		fmt.Println("listen:", err)
		return 1
	}
	fmt.Println("ready")
	for {
		c, err := ln.Accept()
		if err != nil {
			return 0
		}
		_, _ = io.WriteString(c, "inside\n")
		_ = c.Close()
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

// startNamespace runs an echo server in a fresh user+network namespace and
// returns its pid, skipping when this host cannot make one.
func startNamespace(t *testing.T, servePort int) int {
	t.Helper()
	if _, err := exec.LookPath("ip"); err != nil {
		skipOrFail(t, "ip not available")
	}
	if err := exec.Command("unshare", "--user", "--map-root-user", "--net", "true").Run(); err != nil {
		skipOrFail(t, "unprivileged user+net namespaces unavailable: %v", err)
	}
	self, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command("unshare", "--user", "--map-root-user", "--net", "sh", "-c",
		`ip link set lo up && exec "$0"`, self)
	cmd.Env = append(os.Environ(), serveEnv+"="+strconv.Itoa(servePort))
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		skipOrFail(t, "namespace echo server did not start: %q %v", line, err)
	}
	return cmd.Process.Pid
}

func useTestHelper(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	prev := HelperArgv
	HelperArgv = func(port int) ([]string, error) {
		return []string{self, HelperMode, strconv.Itoa(port)}, nil
	}
	t.Cleanup(func() { HelperArgv = prev })
}

func TestDialReachesThePortInsideTheNamespaceOnly(t *testing.T) {
	useTestHelper(t)
	port := freePort(t)
	pid := startNamespace(t, port)
	nsenter, err := exec.LookPath("nsenter")
	if err != nil {
		skipOrFail(t, "nsenter not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := Dial(ctx, nsenter, pid, port)
	require.NoError(t, err)
	line, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "inside\n", line)
	_ = conn.Close()

	// A host listener on another port is not what the namespace sees there.
	host, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = host.Close() }()
	_, err = Dial(ctx, nsenter, pid, host.Addr().(*net.TCPAddr).Port)
	assert.True(t, errors.Is(err, ErrRefused), "got %v", err)
}

func TestRunHelperRejectsBadArgs(t *testing.T) {
	for _, args := range [][]string{
		{"x", HelperMode},
		{"x", HelperMode, "0"},
		{"x", HelperMode, "70000"},
		{"x", HelperMode, "80", "extra"},
		{"x", HelperMode, "localhost:80"},
	} {
		handled, code := RunHelper(args)
		assert.True(t, handled)
		assert.Equal(t, invalidExit, code, args)
	}
	handled, _ := RunHelper([]string{"x", "other"})
	assert.False(t, handled)
}
