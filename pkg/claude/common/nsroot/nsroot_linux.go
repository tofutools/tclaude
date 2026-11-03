//go:build linux

// Package nsroot opens the file system exactly as another process sees it:
// the root of its mount namespace, through /proc/<pid>/root. The live-page
// stage serves a sandboxed agent's static pages through it, so what a page
// serves is what the agent itself can read, whatever its sandbox hides,
// overlays or binds, and wherever it has since renamed things to.
//
// Opening /proc/<pid>/root needs ptrace read access to the process (same
// user, and a dumpable process). Yama's ptrace_scope=1 restricts only ptrace
// attach, not this read access, so the daemon can open a sandbox it did not
// spawn. Any failure is an error: callers fail closed.
package nsroot

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Root is one process's root directory, held open.
type Root struct {
	pid   int
	pidfd int
	fd    int
	ns    string
}

// MountNS is pid's mount namespace identity, as /proc shows it.
func MountNS(pid int) (string, error) {
	return os.Readlink("/proc/" + strconv.Itoa(pid) + "/ns/mnt")
}

// SandboxCandidates picks, from pids in breadth-first order, the processes
// a bubblewrap sandbox runs: each lives in a mount namespace other than the
// caller's, is not bwrap itself, and descends from a bwrap within pids. Only
// those are trusted to show the sandbox's settled root. A bwrap child joins
// its new namespace before it pivots into the sandbox's root, so until it
// execs it still sees the host's; and a helper started beside bwrap (pasta)
// has a namespace of its own that is not the agent's. All candidates share
// the first such namespace; the rest are fallbacks when the first cannot be
// opened.
func SandboxCandidates(pids []int) (ns string, out []int) {
	self, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		return "", nil
	}
	type proc struct {
		comm string
		ppid int
	}
	procs := make(map[int]proc, len(pids))
	for _, pid := range pids {
		if comm, ppid, ok := procStat(pid); ok {
			procs[pid] = proc{comm, ppid}
		}
	}
	underBwrap := func(pid int) bool {
		for seen := 0; seen < len(procs); seen++ {
			p, ok := procs[pid]
			if !ok {
				return false
			}
			parent, ok := procs[p.ppid]
			if !ok {
				return false
			}
			if parent.comm == "bwrap" {
				return true
			}
			pid = p.ppid
		}
		return false
	}
	for _, pid := range pids {
		p, ok := procs[pid]
		if !ok || p.comm == "bwrap" || !underBwrap(pid) {
			continue
		}
		got, err := MountNS(pid)
		if err != nil || got == self {
			continue
		}
		if ns == "" {
			ns = got
		}
		if got == ns {
			out = append(out, pid)
		}
	}
	return ns, out
}

// procStat reads a process's command name and parent pid.
func procStat(pid int) (comm string, ppid int, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", 0, false
	}
	s := string(b)
	open, closing := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || closing < open {
		return "", 0, false
	}
	fields := strings.Fields(s[closing+1:])
	if len(fields) < 2 {
		return "", 0, false
	}
	ppid, err = strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, false
	}
	return s[open+1 : closing], ppid, true
}

// Open opens pid's root. wantNS is the mount namespace pid was found in; a
// pid that has since been reused by another process is refused.
func Open(pid int, wantNS string) (*Root, error) {
	if pid <= 1 || wantNS == "" {
		return nil, fmt.Errorf("nsroot: invalid pid %d", pid)
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, fmt.Errorf("nsroot: pidfd of %d: %w", pid, err)
	}
	r := &Root{pid: pid, pidfd: pidfd, fd: -1, ns: wantNS}
	if ns, err := MountNS(pid); err != nil || ns != wantNS {
		r.Close()
		return nil, errors.New("nsroot: the process left its namespace")
	}
	fd, err := unix.Open("/proc/"+strconv.Itoa(pid)+"/root", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		r.Close()
		return nil, fmt.Errorf("nsroot: open root of %d: %w", pid, err)
	}
	r.fd = fd
	// The pidfd pins the identity: if the process the pid named exited
	// before the open, the pid may name another one now.
	if ns, err := MountNS(pid); err != nil || ns != wantNS || !r.Alive() {
		r.Close()
		return nil, errors.New("nsroot: the process exited while its root was opened")
	}
	return r, nil
}

// Alive reports whether the process is still running.
func (r *Root) Alive() bool {
	return r != nil && r.pidfd >= 0 && unix.PidfdSendSignal(r.pidfd, 0, nil, 0) == nil
}

// NS is the root's mount namespace.
func (r *Root) NS() string { return r.ns }

// OpenDir opens the directory at abs as the process sees it. Symlinks and
// ".." resolve inside the root, never above it, and magic links are refused.
// The caller owns the returned fd.
func (r *Root) OpenDir(abs string) (int, error) {
	if r == nil || r.fd < 0 {
		return -1, errors.New("nsroot: closed")
	}
	how := &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(r.fd, abs, how)
	if err != nil {
		return -1, err
	}
	if !r.Alive() {
		_ = unix.Close(fd)
		return -1, errors.New("nsroot: the process exited")
	}
	return fd, nil
}

// Close releases the root.
func (r *Root) Close() {
	if r == nil {
		return
	}
	if r.fd >= 0 {
		_ = unix.Close(r.fd)
		r.fd = -1
	}
	if r.pidfd >= 0 {
		_ = unix.Close(r.pidfd)
		r.pidfd = -1
	}
}
