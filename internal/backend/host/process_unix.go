//go:build linux || darwin

// Package host owns physical workload and terminal resources for the
// replacement backend. It deliberately knows nothing about platform agents,
// conversations, authorization, or provider protocols.
package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

var ErrProcessIdentityNotLive = errors.New("recorded process identity is not live")

// ProcessSpec is an already-resolved native invocation. Providers construct
// it; the host neither persists nor interprets its arguments or environment.
type ProcessSpec struct {
	Executable string
	Args       []string
	Directory  string
	Env        []string
	// ExactEnvironment prevents ambient daemon credentials and configuration
	// from leaking into a bounded child workload. When false, Env remains an
	// override layer on the current process environment for existing harness
	// providers.
	ExactEnvironment bool
	Stdin            io.Reader
	Stdout           io.Writer
	Stderr           io.Writer
}

// ProcessIdentity is sufficient to reject a recycled PID or process group.
// StartToken is an OS observation, not a platform identity.
type ProcessIdentity struct {
	PID          int    `json:"pid"`
	ProcessGroup int    `json:"process_group"`
	StartToken   string `json:"start_token"`
}

type ProcessObservation struct {
	Running  bool
	Exited   bool
	ExitCode *int
	Unknown  bool
}

// Process is an exact retained or recovered process-group capability.
type Process struct {
	identity ProcessIdentity
	cmd      *exec.Cmd
	done     chan struct{}

	mu       sync.RWMutex
	exitCode *int
}

func StartProcess(spec ProcessSpec) (*Process, error) {
	if strings.TrimSpace(spec.Executable) == "" {
		return nil, fmt.Errorf("executable is required")
	}
	cmd := exec.Command(spec.Executable, spec.Args...)
	cmd.Dir = spec.Directory
	if spec.ExactEnvironment {
		cmd.Env = MergeEnvironment(nil, spec.Env)
	} else {
		cmd.Env = MergeEnvironment(os.Environ(), spec.Env)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, spec.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("read process group: %w", err)
	}
	token, err := processStartToken(pid)
	if err != nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = cmd.Wait()
		return nil, fmt.Errorf("read process start identity: %w", err)
	}
	p := &Process{
		identity: ProcessIdentity{PID: pid, ProcessGroup: pgid, StartToken: token},
		cmd:      cmd,
		done:     make(chan struct{}),
	}
	go p.wait()
	return p, nil
}

// RecoverProcess restores a capability only when the recorded PID, start
// token, and process group still identify the same live workload.
func RecoverProcess(identity ProcessIdentity) (*Process, error) {
	matched, err := processIdentityMatches(identity)
	if err != nil {
		return nil, err
	}
	if !matched {
		return nil, ErrProcessIdentityNotLive
	}
	return &Process{identity: identity}, nil
}

// RecoverProcessByEnvironment finds the one live process carrying a
// provider-private attempt marker and captures its exact identity. It closes
// the crash window between native start and persistence of the returned PID.
func RecoverProcessByEnvironment(key, value string) (*Process, error) {
	if key == "" || value == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
		return nil, fmt.Errorf("invalid process environment marker")
	}
	pids, err := processIDsWithEnvironment(key, value)
	if err != nil {
		return nil, err
	}
	if len(pids) == 0 {
		return nil, ErrProcessIdentityNotLive
	}
	marked := make(map[int]bool, len(pids))
	for _, pid := range pids {
		marked[pid] = true
	}
	roots := make([]int, 0, len(pids))
	for _, pid := range pids {
		parent, parentErr := processParentPID(pid)
		if parentErr == nil && !marked[parent] {
			roots = append(roots, pid)
		}
	}
	if len(roots) != 1 {
		return nil, fmt.Errorf("process environment marker matched %d root processes", len(roots))
	}
	identity, err := identifyProcess(roots[0])
	if err != nil {
		return nil, err
	}
	return RecoverProcess(identity)
}

func identifyProcess(pid int) (ProcessIdentity, error) {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return ProcessIdentity{}, err
	}
	token, err := processStartToken(pid)
	if err != nil {
		return ProcessIdentity{}, err
	}
	return ProcessIdentity{PID: pid, ProcessGroup: pgid, StartToken: token}, nil
}

// OwnsLoopbackPort proves that the exact retained process (or one of its
// descendants) owns the loopback listener before a provider sends secrets.
func (p *Process) OwnsLoopbackPort(port int) (bool, error) {
	matched, err := processIdentityMatches(p.identity)
	if err != nil || !matched {
		return false, err
	}
	return processTreeOwnsLoopbackPort(p.identity.PID, port)
}

func (p *Process) Identity() ProcessIdentity { return p.identity }

func (p *Process) wait() {
	_ = p.cmd.Wait()
	p.mu.Lock()
	if p.cmd.ProcessState != nil {
		code := p.cmd.ProcessState.ExitCode()
		p.exitCode = &code
	}
	p.mu.Unlock()
	close(p.done)
}

func (p *Process) Observe() ProcessObservation {
	if p.cmd != nil {
		select {
		case <-p.done:
			p.mu.RLock()
			defer p.mu.RUnlock()
			return ProcessObservation{Exited: true, ExitCode: cloneInt(p.exitCode)}
		default:
		}
	}
	matched, err := processIdentityMatches(p.identity)
	if err != nil {
		return ProcessObservation{Unknown: true}
	}
	if matched {
		return ProcessObservation{Running: true}
	}
	return ProcessObservation{Exited: true}
}

// Signal sends a signal only after revalidating both the process start token
// and its process group. It never falls back to a stable name or bare PID.
func (p *Process) Signal(signal syscall.Signal) error {
	matched, err := processIdentityMatches(p.identity)
	if err != nil {
		return err
	}
	if !matched {
		return os.ErrProcessDone
	}
	if p.identity.ProcessGroup <= 1 || p.identity.ProcessGroup == syscall.Getpgrp() {
		return fmt.Errorf("refusing unsafe process group %d", p.identity.ProcessGroup)
	}
	if err := syscall.Kill(-p.identity.ProcessGroup, signal); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}

// Stop requests termination, waits for observed exit, then optionally
// escalates. A successful signal dispatch is never reported as exit by itself.
func (p *Process) Stop(ctx context.Context, force bool) (acknowledged, exited bool, err error) {
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	err = p.Signal(signal)
	if errors.Is(err, os.ErrProcessDone) {
		return false, true, nil
	}
	if err != nil {
		return false, false, err
	}
	acknowledged = true
	for {
		observation := p.Observe()
		if observation.Exited {
			return acknowledged, true, nil
		}
		if observation.Unknown {
			return acknowledged, false, fmt.Errorf("process exit is unknown")
		}
		select {
		case <-ctx.Done():
			return acknowledged, false, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func processIdentityMatches(identity ProcessIdentity) (bool, error) {
	if identity.PID <= 1 || identity.ProcessGroup <= 1 || identity.StartToken == "" {
		return false, nil
	}
	pgid, err := syscall.Getpgid(identity.PID)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if pgid != identity.ProcessGroup {
		return false, nil
	}
	token, err := processStartToken(identity.PID)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return token == identity.StartToken, nil
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// MergeEnvironment applies overrides without emitting duplicate keys. This is
// load-bearing for provider-private XDG roots and credentials: libc getenv may
// return the first duplicate rather than the last one.
func MergeEnvironment(base, overrides []string) []string {
	replacements := make(map[string]string, len(overrides))
	order := make([]string, 0, len(overrides))
	for _, entry := range overrides {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if _, exists := replacements[key]; !exists {
			order = append(order, key)
		}
		replacements[key] = entry
	}
	result := make([]string, 0, len(base)+len(replacements))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, replaced := replacements[key]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	for _, key := range order {
		result = append(result, replacements[key])
	}
	return result
}
