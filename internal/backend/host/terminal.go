package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/creack/pty"
)

type SocketIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type TerminalIdentity struct {
	SocketPath string         `json:"socket_path"`
	Socket     SocketIdentity `json:"socket"`
	Session    string         `json:"session"`
	Pane       string         `json:"pane"`
	PanePID    int            `json:"pane_pid"`
	PaneStart  string         `json:"pane_start"`
}

type PreparedTerminalIdentity struct {
	Directory  string `json:"directory"`
	SocketPath string `json:"socket_path"`
	Session    string `json:"session"`
}

type TerminalObservation struct {
	Running  bool
	Exited   bool
	ExitCode *int
	Unknown  bool
}

// TerminalAttachment is the focused host-side view of an attached PTY.
type TerminalAttachment interface {
	io.ReadWriteCloser
	Resize(context.Context, uint16, uint16) error
}

type TerminalHost struct {
	Executable  string
	PrivateRoot string
}

type PreparedTerminal struct {
	host       TerminalHost
	directory  string
	socketPath string
	session    string

	mu       sync.Mutex
	released bool
	aborted  bool
}

type Terminal struct {
	host     TerminalHost
	identity TerminalIdentity
	attached atomic.Int32
}

func (h TerminalHost) Prepare(executionID string) (*PreparedTerminal, error) {
	if strings.TrimSpace(executionID) == "" {
		return nil, fmt.Errorf("execution id is required")
	}
	executable := h.Executable
	if executable == "" {
		executable = "tmux"
	}
	resolved, err := exec.LookPath(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve tmux: %w", err)
	}
	root := filepath.Clean(h.PrivateRoot)
	if root == "." || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("private terminal root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create private terminal root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("protect private terminal root: %w", err)
	}
	nonce, err := randomToken(12)
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(root, "terminal-"+nonce)
	socketPath := filepath.Join(directory, "tmux.sock")
	if len(socketPath) > 100 {
		return nil, fmt.Errorf("private terminal socket path is too long (%d bytes)", len(socketPath))
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, fmt.Errorf("reserve terminal directory: %w", err)
	}
	h.Executable = resolved
	return &PreparedTerminal{
		host: h, directory: directory,
		socketPath: socketPath,
		session:    "exec-" + nonce,
	}, nil
}

func (p *PreparedTerminal) ResourceKey() string { return p.socketPath + ":" + p.session }
func (p *PreparedTerminal) Identity() PreparedTerminalIdentity {
	return PreparedTerminalIdentity{Directory: p.directory, SocketPath: p.socketPath, Session: p.session}
}

func (p *PreparedTerminal) Abort() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released {
		return fmt.Errorf("terminal was released")
	}
	if p.aborted {
		return nil
	}
	p.aborted = true
	return os.Remove(p.directory)
}

func (p *PreparedTerminal) Release(spec ProcessSpec) (*Terminal, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.aborted {
		return nil, fmt.Errorf("terminal was aborted")
	}
	if p.released {
		return nil, fmt.Errorf("terminal was already released")
	}
	if strings.TrimSpace(spec.Executable) == "" {
		return nil, fmt.Errorf("workload executable is required")
	}
	p.released = true
	args := []string{"-S", p.socketPath, "-f", "/dev/null", "new-session", "-d", "-P", "-F", "#{session_name}\t#{pane_id}\t#{pane_pid}", "-s", p.session}
	if spec.Directory != "" {
		args = append(args, "-c", spec.Directory)
	}
	args = append(args, "--", spec.Executable)
	args = append(args, spec.Args...)
	cmd := exec.Command(p.host.Executable, args...)
	if spec.ExactEnvironment {
		cmd.Env = MergeEnvironment(nil, spec.Env)
	} else {
		cmd.Env = MergeEnvironment(os.Environ(), spec.Env)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		terminal, reconcileErr := p.reconcile()
		if terminal != nil {
			return terminal, fmt.Errorf("create private terminal returned uncertain result: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil, errors.Join(fmt.Errorf("create private terminal: %w: %s", err, strings.TrimSpace(string(out))), reconcileErr)
	}
	terminal, err := p.reconcile()
	if err != nil {
		return terminal, fmt.Errorf("discover released terminal after output %q: %w", strings.TrimSpace(string(out)), err)
	}
	return terminal, nil
}

func (p *PreparedTerminal) reconcile() (*Terminal, error) {
	partial := &Terminal{host: p.host, identity: TerminalIdentity{SocketPath: p.socketPath, Session: p.session}}
	socket, err := statSocket(p.socketPath)
	if err != nil {
		return nil, err
	}
	partial.identity.Socket = socket
	out, err := exec.Command(p.host.Executable, "-S", p.socketPath, "list-panes", "-t", "="+p.session,
		"-F", "#{session_name}\t#{pane_id}\t#{pane_pid}").Output()
	if err != nil {
		return partial, err
	}
	parts := strings.Split(strings.TrimRight(string(out), "\r\n"), "\t")
	if len(parts) != 3 || parts[0] != p.session {
		return partial, fmt.Errorf("tmux returned malformed terminal identity %q", strings.TrimSpace(string(out)))
	}
	panePID, err := strconv.Atoi(parts[2])
	if err != nil || panePID <= 1 {
		return partial, fmt.Errorf("tmux returned invalid pane pid %q", parts[2])
	}
	start, err := processStartToken(panePID)
	if err != nil {
		return partial, fmt.Errorf("read pane process identity: %w", err)
	}
	return &Terminal{host: p.host, identity: TerminalIdentity{
		SocketPath: p.socketPath, Socket: socket, Session: p.session,
		Pane: parts[1], PanePID: panePID, PaneStart: start,
	}}, nil
}

func RecoverTerminal(h TerminalHost, identity TerminalIdentity) (*Terminal, error) {
	if h.Executable == "" {
		h.Executable = "tmux"
	}
	resolved, err := exec.LookPath(h.Executable)
	if err != nil {
		return nil, err
	}
	h.Executable = resolved
	t := &Terminal{host: h, identity: identity}
	observation := t.Observe()
	if observation.Unknown {
		return nil, fmt.Errorf("terminal identity is unprovable")
	}
	if observation.Exited {
		return nil, os.ErrProcessDone
	}
	return t, nil
}

// RecoverPreparedTerminal reconciles the identity persisted before Release.
// It finds an exact session if start happened before the backend crashed, and
// reports absence when release never reached the native effect.
func RecoverPreparedTerminal(h TerminalHost, identity PreparedTerminalIdentity) (*Terminal, error) {
	if h.Executable == "" {
		h.Executable = "tmux"
	}
	resolved, err := exec.LookPath(h.Executable)
	if err != nil {
		return nil, err
	}
	h.Executable = resolved
	root := filepath.Clean(h.PrivateRoot)
	if !pathWithin(root, identity.Directory) || filepath.Clean(identity.SocketPath) != filepath.Join(filepath.Clean(identity.Directory), "tmux.sock") {
		return nil, fmt.Errorf("prepared terminal evidence is outside private storage")
	}
	prepared := &PreparedTerminal{host: h, directory: identity.Directory, socketPath: identity.SocketPath, session: identity.Session, released: true}
	return prepared.reconcile()
}

func (t *Terminal) Identity() TerminalIdentity { return t.identity }
func (t *Terminal) AttachmentActive() bool     { return t.attached.Load() > 0 }

func (t *Terminal) Observe() TerminalObservation {
	if t.identity.SocketPath == "" || t.identity.Session == "" || t.identity.Pane == "" ||
		t.identity.PanePID <= 1 || t.identity.PaneStart == "" {
		return TerminalObservation{Unknown: true}
	}
	matched, err := t.socketMatches()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TerminalObservation{Exited: true}
		}
		return TerminalObservation{Unknown: true}
	}
	if !matched {
		return TerminalObservation{Unknown: true}
	}
	out, err := t.tmux("display-message", "-p", "-t", t.identity.Pane,
		"#{session_name}\t#{pane_id}\t#{pane_pid}\t#{pane_dead}\t#{pane_dead_status}").Output()
	if err != nil {
		// tmux deliberately leaves a stale socket entry when the last session
		// exits. The pane process start token, not pathname existence, proves
		// whether the selected workload is gone or merely unobservable.
		start, startErr := processStartToken(t.identity.PanePID)
		if errors.Is(startErr, os.ErrNotExist) || errors.Is(startErr, syscall.ESRCH) ||
			(startErr == nil && start != t.identity.PaneStart) {
			return TerminalObservation{Exited: true}
		}
		return TerminalObservation{Unknown: true}
	}
	parts := strings.Split(strings.TrimRight(string(out), "\r\n"), "\t")
	if len(parts) != 5 || parts[0] != t.identity.Session || parts[1] != t.identity.Pane {
		return TerminalObservation{Unknown: true}
	}
	pid, err := strconv.Atoi(parts[2])
	if err != nil || pid != t.identity.PanePID {
		return TerminalObservation{Unknown: true}
	}
	if parts[3] == "1" {
		code, parseErr := strconv.Atoi(parts[4])
		if parseErr != nil {
			return TerminalObservation{Exited: true}
		}
		return TerminalObservation{Exited: true, ExitCode: &code}
	}
	start, err := processStartToken(pid)
	if err != nil || start != t.identity.PaneStart {
		return TerminalObservation{Unknown: true}
	}
	return TerminalObservation{Running: true}
}

// SendLiteral uses a private tmux buffer and bracketed paste, so arbitrary
// printable/multiline user text is not parsed as tmux keys or shell syntax.
// Submission is a separate effect; any failure after the paste begins is
// intentionally returned to the provider as acceptance-unknown.
func (t *Terminal) SendLiteral(ctx context.Context, text string) error {
	if observation := t.Observe(); !observation.Running {
		return fmt.Errorf("terminal is not an exact running workload")
	}
	buffer, err := randomToken(8)
	if err != nil {
		return err
	}
	load := t.tmuxContext(ctx, "load-buffer", "-b", buffer, "-")
	load.Stdin = strings.NewReader(text)
	if out, err := load.CombinedOutput(); err != nil {
		return fmt.Errorf("load terminal input: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := t.tmuxContext(ctx, "paste-buffer", "-d", "-p", "-b", buffer, "-t", t.identity.Pane).CombinedOutput(); err != nil {
		return fmt.Errorf("paste terminal input: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := t.tmuxContext(ctx, "send-keys", "-t", t.identity.Pane, "Enter").CombinedOutput(); err != nil {
		return fmt.Errorf("submit terminal input: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (t *Terminal) Attach(ctx context.Context) (TerminalAttachment, error) {
	if observation := t.Observe(); observation.Unknown || observation.Exited {
		return nil, fmt.Errorf("terminal is not attachable")
	}
	cmd := exec.CommandContext(ctx, t.host.Executable, "-S", t.identity.SocketPath,
		"attach-session", "-t", "="+t.identity.Session)
	file, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	t.attached.Add(1)
	return &terminalAttachment{file: file, cmd: cmd, attached: &t.attached}, nil
}

func (t *Terminal) Stop(ctx context.Context, force bool) (acknowledged, exited bool, err error) {
	observation := t.Observe()
	if observation.Exited {
		return false, true, nil
	}
	if observation.Unknown {
		return false, false, fmt.Errorf("terminal identity is unprovable")
	}
	if force {
		out, killErr := t.tmuxContext(ctx, "kill-session", "-t", "="+t.identity.Session).CombinedOutput()
		if killErr != nil {
			return false, false, fmt.Errorf("kill terminal session: %w: %s", killErr, strings.TrimSpace(string(out)))
		}
	} else if err := t.SendLiteral(ctx, "/exit"); err != nil {
		return false, false, err
	}
	return true, t.Observe().Exited, nil
}

func (t *Terminal) socketMatches() (bool, error) {
	identity, err := statSocket(t.identity.SocketPath)
	if err != nil {
		return false, err
	}
	return identity == t.identity.Socket, nil
}

func (t *Terminal) tmux(args ...string) *exec.Cmd {
	return exec.Command(t.host.Executable, append([]string{"-S", t.identity.SocketPath}, args...)...)
}

func (t *Terminal) tmuxContext(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, t.host.Executable, append([]string{"-S", t.identity.SocketPath}, args...)...)
}

type terminalAttachment struct {
	file     *os.File
	cmd      *exec.Cmd
	attached *atomic.Int32
	once     sync.Once
}

func (a *terminalAttachment) Read(buffer []byte) (int, error)  { return a.file.Read(buffer) }
func (a *terminalAttachment) Write(buffer []byte) (int, error) { return a.file.Write(buffer) }
func (a *terminalAttachment) Resize(ctx context.Context, columns, rows uint16) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if columns == 0 || rows == 0 || columns > 1000 || rows > 1000 {
		return fmt.Errorf("terminal size must be between 1 and 1000 columns and rows")
	}
	if err := pty.Setsize(a.file, &pty.Winsize{Cols: columns, Rows: rows}); err != nil {
		return fmt.Errorf("resize terminal PTY: %w", err)
	}
	return nil
}
func (a *terminalAttachment) Close() error {
	var closeErr error
	a.once.Do(func() {
		closeErr = a.file.Close()
		if a.cmd.Process != nil {
			_ = a.cmd.Process.Signal(syscall.SIGTERM)
		}
		_ = a.cmd.Wait()
		a.attached.Add(-1)
	})
	return closeErr
}

func statSocket(path string) (SocketIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return SocketIdentity{}, err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return SocketIdentity{}, fmt.Errorf("terminal socket path is not a socket")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return SocketIdentity{}, fmt.Errorf("terminal socket identity is unavailable")
	}
	return SocketIdentity{Device: uint64(stat.Dev), Inode: stat.Ino}, nil
}

func randomToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
