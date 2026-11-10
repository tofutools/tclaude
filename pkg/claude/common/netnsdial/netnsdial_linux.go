//go:build linux

package netnsdial

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// HelperMode is the private argv token of the in-namespace helper. It shares
// the prefix probehelper dispatches on before any command construction.
const HelperMode = "__tclaude_stacked_probe_netns_dial"

const (
	helperDialTimeout = 5 * time.Second
	invalidExit       = 64
	failureExit       = 74
)

// ErrRefused reports that nothing accepted the connection inside the
// namespace (ECONNREFUSED there).
var ErrRefused = errors.New("connection refused inside the namespace")

// HelperArgv returns the argv (program first) nsenter execs inside the
// namespace. Tests replace it to point at their own binary.
var HelperArgv = func(port int) ([]string, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("netns dial helper executable: %w", err)
	}
	return []string{self, HelperMode, strconv.Itoa(port)}, nil
}

// Dial connects to 127.0.0.1:port inside the network namespace of pid.
// nsenterPath must be a trusted nsenter executable.
func Dial(ctx context.Context, nsenterPath string, pid, port int) (net.Conn, error) {
	if pid <= 1 || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("netns dial: invalid pid %d or port %d", pid, port)
	}
	if nsenterPath == "" {
		return nil, errors.New("netns dial: nsenter is unavailable")
	}
	netFD, err := unix.Open("/proc/"+strconv.Itoa(pid)+"/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open network namespace of pid %d: %w", pid, err)
	}
	netFile := os.NewFile(uintptr(netFD), "netns")
	defer func() { _ = netFile.Close() }()
	// The user namespace that OWNS the network namespace, not the process's
	// own (possibly nested) one: that is where the capability over the netns
	// lives.
	ownerFD, err := unix.IoctlRetInt(netFD, unix.NS_GET_USERNS)
	if err != nil {
		return nil, fmt.Errorf("resolve owning user namespace of pid %d: %w", pid, err)
	}
	ownerFile := os.NewFile(uintptr(ownerFD), "netns-owner-userns")
	defer func() { _ = ownerFile.Close() }()

	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("netns dial socketpair: %w", err)
	}
	parent := os.NewFile(uintptr(pair[0]), "netns-dial-parent")
	child := os.NewFile(uintptr(pair[1]), "netns-dial-child")
	defer func() { _ = parent.Close() }()

	helper, err := HelperArgv(port)
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	// ExtraFiles land at fd 3 (socket), 4 (owner userns), 5 (netns).
	// --preserve-credentials: the sandbox userns denies setgroups, so
	// nsenter's default setgroups() would fail.
	args := append([]string{
		"--preserve-credentials",
		"--user=/proc/self/fd/4",
		"--net=/proc/self/fd/5",
		"--",
	}, helper...)
	ctx, cancel := context.WithTimeout(ctx, helperDialTimeout+5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nsenterPath, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	cmd.ExtraFiles = []*os.File{child, ownerFile, netFile}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = child.Close()
		return nil, fmt.Errorf("start nsenter: %w", err)
	}
	_ = child.Close()

	conn, recvErr := receiveConn(ctx, parent, port)
	waitErr := cmd.Wait()
	if recvErr != nil {
		if errors.Is(recvErr, ErrRefused) {
			return nil, recvErr
		}
		if waitErr != nil {
			return nil, fmt.Errorf("netns dial helper: %v: %s", waitErr, strings.TrimSpace(stderr.String()))
		}
		return nil, recvErr
	}
	return conn, nil
}

// receiveConn reads the helper's answer: one byte plus the socket on
// success, or a short error text.
func receiveConn(ctx context.Context, parent *os.File, wantPort int) (net.Conn, error) {
	if dl, ok := ctx.Deadline(); ok {
		_ = parent.SetReadDeadline(dl)
	}
	raw, err := parent.SyscallConn()
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 256)
	oob := make([]byte, unix.CmsgSpace(4))
	var n, oobn, flags int
	var rerr error
	if err := raw.Read(func(fd uintptr) bool {
		n, oobn, flags, _, rerr = unix.Recvmsg(int(fd), buf, oob, unix.MSG_CMSG_CLOEXEC)
		return rerr != unix.EAGAIN
	}); err != nil {
		return nil, fmt.Errorf("netns dial: read helper answer: %w", err)
	}
	if rerr != nil {
		return nil, fmt.Errorf("netns dial: read helper answer: %w", rerr)
	}
	msg := string(buf[:n])
	if oobn == 0 {
		if msg == "refused" {
			return nil, ErrRefused
		}
		if msg == "" {
			return nil, errors.New("netns dial: helper exited without an answer")
		}
		return nil, fmt.Errorf("netns dial: %s", msg)
	}
	cmsgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	var fds []int
	for i := range cmsgs {
		if got, err := unix.ParseUnixRights(&cmsgs[i]); err == nil {
			fds = append(fds, got...)
		}
	}
	if err != nil || len(cmsgs) != 1 || len(fds) != 1 || flags&unix.MSG_CTRUNC != 0 || msg != "ok" {
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
		return nil, errors.New("netns dial: malformed helper answer")
	}
	f := os.NewFile(uintptr(fds[0]), "netns-dial-conn")
	defer func() { _ = f.Close() }()
	// Only a connected TCP socket is accepted back.
	if typ, err := unix.GetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_TYPE); err != nil || typ != unix.SOCK_STREAM {
		return nil, errors.New("netns dial: helper returned a non-stream socket")
	}
	conn, err := net.FileConn(f)
	if err != nil {
		return nil, fmt.Errorf("netns dial: adopt socket: %w", err)
	}
	if _, ok := conn.(*net.TCPConn); !ok {
		_ = conn.Close()
		return nil, errors.New("netns dial: helper returned a non-TCP socket")
	}
	// The socket must be connected to exactly what was asked for.
	if peer, ok := conn.RemoteAddr().(*net.TCPAddr); !ok || !peer.IP.Equal(net.IPv4(127, 0, 0, 1)) || peer.Port != wantPort {
		_ = conn.Close()
		return nil, errors.New("netns dial: helper returned a socket to the wrong peer")
	}
	return conn, nil
}

// RunHelper is the in-namespace helper: args are the full argv. It reports
// whether args named the helper mode, and the exit code.
func RunHelper(args []string) (bool, int) {
	if len(args) < 2 || args[1] != HelperMode {
		return false, 0
	}
	if len(args) != 3 {
		return true, invalidExit
	}
	port, err := strconv.Atoi(args[2])
	if err != nil || port <= 0 || port > 65535 {
		return true, invalidExit
	}
	const sock = 3
	reply := func(msg string, rights []byte) int {
		if err := unix.Sendmsg(sock, []byte(msg), rights, nil, 0); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "netns dial helper: send: %v\n", err)
			return failureExit
		}
		return 0
	}
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), helperDialTimeout)
	if err != nil {
		if errors.Is(err, unix.ECONNREFUSED) {
			return true, reply("refused", nil)
		}
		return true, reply("dial failed", nil)
	}
	f, err := conn.(*net.TCPConn).File()
	_ = conn.Close()
	if err != nil {
		return true, reply("dial failed", nil)
	}
	defer func() { _ = f.Close() }()
	return true, reply("ok", unix.UnixRights(int(f.Fd())))
}
