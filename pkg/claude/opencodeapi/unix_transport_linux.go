//go:build linux

package opencodeapi

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"golang.org/x/sys/unix"
)

var afterUnixConnectForTest func()

// CreateUnixListener creates a new control socket without replacing anything
// already present at the authority path.
func CreateUnixListener(path string) (*net.UnixListener, int64, int64, error) {
	parentDevice, parentInode, err := controlParentIdentity(path)
	if err != nil {
		return nil, 0, 0, err
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, 0, 0, fmt.Errorf("OpenCode control socket target already exists")
	} else if !os.IsNotExist(err) {
		return nil, 0, 0, fmt.Errorf("inspect OpenCode control socket target: %w", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, 0, 0, fmt.Errorf("bind OpenCode control socket: %w", err)
	}
	// The parent closes its copy immediately after exec. The inherited relay
	// remains authoritative, so pathname cleanup must be the explicit
	// recorded-inode operation rather than net.UnixListener's close-time unlink.
	listener.SetUnlinkOnClose(false)
	expectedDevice, expectedInode, err := socketPathIdentity(path, false)
	if err != nil {
		_ = listener.Close()
		return nil, 0, 0, err
	}
	fail := func(err error) (*net.UnixListener, int64, int64, error) {
		_ = listener.Close()
		_ = unlinkSocketIdentity(path, expectedDevice, expectedInode, false)
		return nil, 0, 0, err
	}
	afterParentDevice, afterParentInode, err := controlParentIdentity(path)
	if err != nil {
		return fail(err)
	}
	if afterParentDevice != parentDevice || afterParentInode != parentInode {
		return fail(fmt.Errorf("OpenCode control socket parent was replaced during creation"))
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fail(fmt.Errorf("chmod OpenCode control socket: %w", err))
	}
	device, inode, err := controlSocketIdentity(path)
	if err != nil {
		return fail(err)
	}
	if device != expectedDevice || inode != expectedInode {
		return fail(fmt.Errorf("OpenCode control socket was replaced during creation"))
	}
	return listener, device, inode, nil
}

// ExecUnixRelayLaunch binds the control listener in the direct child whose PID
// agentd records, reports its replay authority, waits for durable persistence,
// then execs bubblewrap without changing PID. The accepted connection's
// SO_PEERCRED therefore names the recorded runtime root rather than agentd.
func ExecUnixRelayLaunch(path string, command []string) error {
	if len(command) == 0 {
		return fmt.Errorf("OpenCode Unix launcher has no bubblewrap command")
	}
	status := os.NewFile(3, "opencode-unix-launch-status")
	gate := os.NewFile(4, "opencode-unix-launch-gate")
	if status == nil || gate == nil {
		return fmt.Errorf("OpenCode Unix launcher has incomplete handshake descriptors")
	}
	listener, device, inode, err := CreateUnixListener(path)
	if err != nil {
		return err
	}
	runtime := db.OpenCodeRuntime{
		Transport: db.OpenCodeTransportUnixRelay, ControlSocketPath: path,
		ControlSocketDevice: device, ControlSocketInode: inode,
	}
	fail := func(err error) error {
		_ = listener.Close()
		_ = RemoveUnixSocket(runtime)
		_ = status.Close()
		_ = gate.Close()
		return err
	}
	listenerFile, err := listener.File()
	if err != nil {
		return fail(fmt.Errorf("duplicate OpenCode Unix launch listener: %w", err))
	}
	defer listenerFile.Close()
	selfPath, err := os.Executable()
	if err != nil {
		return fail(fmt.Errorf("resolve OpenCode Unix launcher executable: %w", err))
	}
	selfFile, err := os.Open(selfPath)
	if err != nil {
		return fail(fmt.Errorf("open OpenCode Unix launcher executable: %w", err))
	}
	defer selfFile.Close()
	if err := WriteUnixLaunchAuthority(status, UnixLaunchAuthority{
		Device: device, Inode: inode,
	}); err != nil {
		return fail(err)
	}
	_ = status.Close()
	var acknowledgement [1]byte
	if _, err := io.ReadFull(gate, acknowledgement[:]); err != nil ||
		acknowledgement[0] != 1 {
		return fail(fmt.Errorf(
			"OpenCode Unix launch authority was not durably acknowledged"))
	}
	_ = gate.Close()
	if err := unix.Dup3(int(listenerFile.Fd()), 3, 0); err != nil {
		return fail(fmt.Errorf("install OpenCode Unix listener as fd 3: %w", err))
	}
	if err := unix.Dup3(int(selfFile.Fd()), 4, 0); err != nil {
		_ = unix.Close(3)
		return fail(fmt.Errorf("install OpenCode Unix relay executable as fd 4: %w", err))
	}
	_ = listenerFile.Close()
	_ = selfFile.Close()
	_ = listener.Close()
	if err := unix.Exec(command[0], command, os.Environ()); err != nil {
		_ = unix.Close(3)
		_ = unix.Close(4)
		return fail(fmt.Errorf("exec bubblewrap for OpenCode Unix relay: %w", err))
	}
	return nil
}

// RemoveUnixSocket removes only the exact inode recorded for this runtime.
func RemoveUnixSocket(runtime db.OpenCodeRuntime) error {
	if runtime.Transport != db.OpenCodeTransportUnixRelay {
		return nil
	}
	if err := validateControlParent(runtime.ControlSocketPath); err != nil {
		return err
	}
	device, inode, err := controlSocketIdentity(runtime.ControlSocketPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if device != runtime.ControlSocketDevice || inode != runtime.ControlSocketInode {
		return fmt.Errorf("refusing to remove replaced OpenCode control socket")
	}
	return unlinkSocketIdentity(runtime.ControlSocketPath,
		runtime.ControlSocketDevice, runtime.ControlSocketInode, true)
}

func unlinkSocketIdentity(path string, device, inode int64, requireMode bool) error {
	parentFD, err := unix.Open(filepath.Dir(path),
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open OpenCode control parent for cleanup: %w", err)
	}
	defer func() { _ = unix.Close(parentFD) }()
	var stat unix.Stat_t
	if err := unix.Fstatat(parentFD, filepath.Base(path),
		&stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if err == unix.ENOENT {
			return nil
		}
		return fmt.Errorf("recheck OpenCode control socket for cleanup: %w", err)
	}
	if int64(stat.Dev) != device || int64(stat.Ino) != inode ||
		stat.Mode&unix.S_IFMT != unix.S_IFSOCK ||
		(requireMode && stat.Mode&0o777 != 0o600) {
		return fmt.Errorf("refusing to remove replaced OpenCode control socket")
	}
	if err := unix.Unlinkat(parentFD, filepath.Base(path), 0); err != nil {
		return fmt.Errorf("remove OpenCode control socket: %w", err)
	}
	return nil
}

func doUnixRequest(client *http.Client, request *http.Request, runtime db.OpenCodeRuntime) (*http.Response, error) {
	clone := *client
	var transport *http.Transport
	if existing, ok := client.Transport.(*http.Transport); ok {
		transport = existing.Clone()
	} else {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	transport.Proxy = nil
	// A fresh connection re-runs pathname + peer proof for every request.
	// Keeping an idle connection would skip DialContext after a path swap.
	transport.DisableKeepAlives = true
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialVerifiedUnix(ctx, runtime)
	}
	clone.Transport = transport
	return clone.Do(request)
}

func dialVerifiedUnix(ctx context.Context, runtime db.OpenCodeRuntime) (net.Conn, error) {
	if err := db.ValidateOpenCodeRuntimeTransport(runtime); err != nil {
		return nil, err
	}
	beforeDevice, beforeInode, err := controlSocketIdentity(runtime.ControlSocketPath)
	if err != nil {
		return nil, err
	}
	if beforeDevice != runtime.ControlSocketDevice || beforeInode != runtime.ControlSocketInode {
		return nil, fmt.Errorf("OpenCode control socket identity changed before connect")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", runtime.ControlSocketPath)
	if err != nil {
		return nil, err
	}
	if afterUnixConnectForTest != nil {
		afterUnixConnectForTest()
	}
	fail := func(err error) (net.Conn, error) {
		_ = conn.Close()
		return nil, err
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return fail(fmt.Errorf("OpenCode control connection is not Unix"))
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return fail(err)
	}
	var credential *syscall.Ucred
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		credential, credentialErr = syscall.GetsockoptUcred(
			int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return fail(err)
	}
	if credentialErr != nil {
		return fail(fmt.Errorf("prove OpenCode control peer credentials: %w", credentialErr))
	}
	if credential == nil || credential.Uid != uint32(os.Geteuid()) ||
		!ProcessInSubtree(runtime.PID, int(credential.Pid)) {
		return fail(fmt.Errorf("OpenCode control peer is outside the recorded process subtree"))
	}
	afterDevice, afterInode, err := controlSocketIdentity(runtime.ControlSocketPath)
	if err != nil {
		return fail(err)
	}
	if afterDevice != beforeDevice || afterInode != beforeInode {
		return fail(fmt.Errorf("OpenCode control socket identity changed during connect"))
	}
	return conn, nil
}

func validateControlParent(path string) error {
	_, _, err := controlParentIdentity(path)
	return err
}

func controlParentIdentity(path string) (int64, int64, error) {
	cleanPath := filepath.Clean(path)
	if cleanPath != path || !filepath.IsAbs(cleanPath) ||
		filepath.Base(cleanPath) != "control.sock" {
		return 0, 0, fmt.Errorf("invalid OpenCode control socket path")
	}
	path = cleanPath
	if len(path) >= len(unix.RawSockaddrUnix{}.Path) {
		return 0, 0, fmt.Errorf("OpenCode control socket path exceeds Linux sockaddr capacity")
	}
	parent := filepath.Dir(path)
	agentID := filepath.Base(parent)
	if !strings.HasPrefix(agentID, "agt_") || len(agentID) != len("agt_")+32 {
		return 0, 0, fmt.Errorf("OpenCode control socket parent has invalid agent identity")
	}
	for _, r := range strings.TrimPrefix(agentID, "agt_") {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return 0, 0, fmt.Errorf("OpenCode control socket parent has invalid agent identity")
		}
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return 0, 0, fmt.Errorf("inspect OpenCode control socket parent: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() ||
		info.Mode().Perm() != 0o700 {
		return 0, 0, fmt.Errorf("OpenCode control socket parent must be a real mode-0700 directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return 0, 0, fmt.Errorf("OpenCode control socket parent has the wrong owner")
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil || resolved != parent {
		return 0, 0, fmt.Errorf("OpenCode control socket parent is not canonical")
	}
	return int64(stat.Dev), int64(stat.Ino), nil
}

func controlSocketIdentity(path string) (int64, int64, error) {
	return socketPathIdentity(path, true)
}

func socketPathIdentity(path string, requireMode bool) (int64, int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 ||
		(requireMode && info.Mode().Perm() != 0o600) {
		return 0, 0, fmt.Errorf("OpenCode control authority is not a real mode-0600 socket")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return 0, 0, fmt.Errorf("OpenCode control socket has the wrong owner")
	}
	return int64(stat.Dev), int64(stat.Ino), nil
}
