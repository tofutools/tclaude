package agentd

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/nsroot"
	"github.com/tofutools/tclaude/pkg/claude/common/portowner"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"golang.org/x/sys/unix"
)

// An agent's view of the file system, for static live pages (tcl-kxiqqd).
//
// What a page serves can come back to its agent (`page marks`, `page
// snapshot`, `page dom`), so a static page's content goes back only when the
// stage serves exactly what the agent itself can read:
//
//   - A Linux tclaude-layer agent's page is served through the agent's own
//     mount namespace (/proc/<pid>/root, see nsroot): its deny rules, tmpfs
//     overlays, binds and constructed root apply by construction, wherever
//     the agent has since renamed things. If that view cannot be opened the
//     page serves nothing (it fails closed).
//   - An unconfined agent (an "off" or "resource-only" launch, or a
//     harness-builtin launch whose harness sandbox is recorded off) reads
//     what the daemon reads.
//
// Everything else is served to the operator, never read back: a
// harness-builtin launch whose harness enforces its own rules (which the
// stage cannot see), a macOS Seatbelt launch (whose path rules a name check
// can only approximate), or a launch with nothing recorded. Those pages still
// leave out what the recorded profile hides, by path, as defence in depth.
// The protected roots and the tmux socket directory are left out for every
// agent.

var pageFSViewRecheck = 5 * time.Second // a var for tests

// pageFSClass is how a page's files reach the stage.
type pageFSClass int

const (
	// pageFSGuarded: the daemon's view, minus what the recorded profile
	// hides by path. Not read back.
	pageFSGuarded pageFSClass = iota
	// pageFSUnconfined: the daemon's view, which is the agent's.
	pageFSUnconfined
	// pageFSSandboxRoot: the agent's own mount namespace.
	pageFSSandboxRoot
)

// pageFSView is one generation's view.
type pageFSView struct {
	class pageFSClass
	// constructed says paths no entry binds are absent (a constructed root).
	constructed bool
	entries     []pageFSEntry
	// always are hidden whatever the class.
	always []string
}

// pageFSEntry is one mount entry, by its sandbox path in every spelling the
// host resolves it to.
type pageFSEntry struct {
	paths  []string
	hidden bool
}

// pageFSSpellings is a host path as written and with symlinks resolved, so
// a plan written through a symlinked home still matches the stage's canonical
// paths.
func pageFSSpellings(p string) []string {
	clean := filepath.Clean(p)
	out := []string{clean}
	if real, err := filepath.EvalSymlinks(clean); err == nil && real != clean {
		out = append(out, real)
	}
	return out
}

// pageFSFold is how paths compare: case-insensitively on macOS, whose file
// systems usually are, so a differently-cased request still meets its rule.
func pageFSFold(p string) string {
	if runtime.GOOS == "darwin" {
		return strings.ToLower(p)
	}
	return p
}

func pageFSUnder(p, root string) bool {
	p, root = pageFSFold(p), pageFSFold(root)
	if root == "/" {
		return true
	}
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// readBack says whether the view's content may go back to the agent.
func (v *pageFSView) readBack() bool {
	return v != nil && v.class != pageFSGuarded
}

// hidden reports whether the stage leaves abs (a canonical host path) out.
// In a guarded view the last entry that covers abs decides, as the later
// mount does.
func (v *pageFSView) hidden(abs string) bool {
	if v == nil {
		return false
	}
	for _, root := range v.always {
		if pageFSUnder(abs, root) {
			return true
		}
	}
	if v.class != pageFSGuarded {
		return false
	}
	decided, hide := false, false
	for _, e := range v.entries {
		for _, p := range e.paths {
			if pageFSUnder(abs, p) {
				decided, hide = true, e.hidden
				break
			}
		}
	}
	if !decided {
		return v.constructed
	}
	return hide
}

// pageFSHarnessWallOff says a harness-builtin launch recorded its harness's
// own sandbox as off, so nothing but the daemon's view confines it.
func pageFSHarnessWallOff(row *db.SessionRow) bool {
	h, ok := harness.Get(row.Harness)
	if !ok {
		return false
	}
	if h.Name == harness.DefaultName {
		// Claude Code's launch request ("inherit") says nothing; the
		// launch-time verdict does.
		return row.OSSandboxState == harness.ClaudeSandboxOff && !row.OSSandboxUnverified
	}
	// The off mode is the launch request; a recorded verdict can overrule it
	// (Codex managed requirements keep its sandbox on, or may:
	// session.VerifyCodexSandboxOff).
	off, err := harness.SandboxOffMode(h)
	return err == nil && row.HarnessBuiltinMode == off && row.OSSandboxState == "" && !row.OSSandboxUnverified
}

// pageFSClaudeVerdictOff says a launch is not one Claude Code still
// confines: a managed policy can keep its sandbox on even when tclaude
// launches it with none, and then only the launch-time verdict tells.
func pageFSClaudeVerdictOff(row *db.SessionRow) bool {
	h, ok := harness.Get(row.Harness)
	if ok && h.Name != harness.DefaultName {
		return true
	}
	return row.OSSandboxState == harness.ClaudeSandboxOff && !row.OSSandboxUnverified
}

// sameAs says two views serve and read back alike.
func (v *pageFSView) sameAs(o *pageFSView) bool {
	if v.class != o.class || v.constructed != o.constructed || len(v.entries) != len(o.entries) || len(v.always) != len(o.always) {
		return false
	}
	for i := range v.entries {
		if v.entries[i].hidden != o.entries[i].hidden || strings.Join(v.entries[i].paths, "\x00") != strings.Join(o.entries[i].paths, "\x00") {
			return false
		}
	}
	return strings.Join(v.always, "\x00") == strings.Join(o.always, "\x00")
}

// pageFSViewFor builds the view of the session behind convID, whose startup
// directory is worktree (in each spelling).
func pageFSViewFor(convID string, worktree []string) *pageFSView {
	v := &pageFSView{}
	protected, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return v
	}
	for _, p := range protected {
		v.always = append(v.always, pageFSSpellings(p)...)
	}
	if dir, err := clcommon.TclaudeTmuxSocketDir(); err == nil {
		v.always = append(v.always, pageFSSpellings(dir)...)
	}
	row, err := db.FindSessionByConvID(convID)
	if err != nil || row == nil {
		return v
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(row.SandboxImplementation)
	if err != nil {
		return v
	}
	switch {
	case implementation.OmitsOSConfinement() && pageFSClaudeVerdictOff(row):
		v.class = pageFSUnconfined
		return v
	case implementation == sandboxpolicy.ImplementationTclaudeLayer && runtime.GOOS == "linux":
		v.class = pageFSSandboxRoot
		return v
	case implementation == sandboxpolicy.ImplementationHarnessBuiltin && pageFSHarnessWallOff(row):
		v.class = pageFSUnconfined
		return v
	}
	if row.EffectiveSandbox == nil {
		return v
	}
	plan, err := sandboxpolicy.RenderMountPlan(row.EffectiveSandbox.Effective)
	if err != nil {
		return v
	}
	v.constructed = plan.EffectiveRootPosture() == sandboxpolicy.RootConstructed
	if v.constructed {
		// The launch contract always binds the startup directory.
		v.entries = append(v.entries, pageFSEntry{paths: worktree})
	}
	for _, e := range plan.Entries {
		hidden := e.Mode == sandboxpolicy.MountHide || e.Mode == sandboxpolicy.MountTmpfs || e.IsRemapped()
		v.entries = append(v.entries, pageFSEntry{paths: pageFSSpellings(e.Path), hidden: hidden})
	}
	return v
}

// pageFSRoot is an agent's mount namespace, held open (nsroot.Root; a seam
// for tests).
type pageFSRoot interface {
	Alive() bool
	NS() string
	OpenDir(abs string) (int, error)
	Close()
}

var errPageFSNoSandbox = errors.New("the agent's sandbox is not running")

// pageFSOpenSandboxRoot opens the root of the agent's sandbox: the
// shallowest process in its tree that lives in another mount namespace.
var pageFSOpenSandboxRoot = func(agentID string) (pageFSRoot, error) {
	proc, err := stageResolveAgent(agentID)
	if err != nil {
		return nil, err
	}
	ns, pids := nsroot.SandboxCandidates(portowner.ProcessSubtree(proc.root))
	if len(pids) == 0 {
		return nil, errPageFSNoSandbox
	}
	err = errPageFSNoSandbox
	for _, pid := range pids {
		var root *nsroot.Root
		if root, err = nsroot.Open(pid, ns); err == nil {
			return root, nil
		}
	}
	return nil, err
}

// pageFSViewer follows an agent's current generation and, for a sandbox-root
// view, holds the agent's mount namespace open. gen counts views: it moves
// when the agent's conversation or sandbox changes, and the page reloads.
type pageFSViewer struct {
	agentID  string
	worktree []string // the worktree's spellings, as the sandbox may show it
	onChange func()   // reloads the page; set before first use

	mu      sync.Mutex
	conv    string
	view    *pageFSView
	checked time.Time
	gen     uint64
	shown   uint64 // the gen the page's last document was served under
	root    pageFSRoot
	dir     int    // the worktree inside root, or -1
	lastNS  string // the namespace last held, kept across a failed reopen
	closed  bool
}

func newPageFSViewer(agentID, convID, worktree string) *pageFSViewer {
	spellings := pageFSSpellings(worktree)
	return &pageFSViewer{
		agentID: agentID, worktree: spellings, conv: convID,
		view: pageFSViewFor(convID, spellings), checked: time.Now(), gen: 1, dir: -1,
	}
}

// changedLocked moves to a new generation; the caller reloads the page.
func (r *pageFSViewer) changedLocked() func() {
	r.gen++
	r.dropRootLocked()
	return r.onChange
}

func (r *pageFSViewer) dropRootLocked() {
	if r.dir >= 0 {
		_ = unix.Close(r.dir)
		r.dir = -1
	}
	if r.root != nil {
		r.root.Close()
		r.root = nil
	}
}

// refreshLocked rechecks the agent's generation every few seconds (always,
// when force) and, for a sandbox-root view, that the sandbox it holds still
// runs (reopening it when not). It returns a reload to run once the lock is
// released.
func (r *pageFSViewer) refreshLocked(force bool) (reload func(), err error) {
	if r.closed {
		return nil, errors.New("this page's source has closed")
	}
	if force || time.Since(r.checked) >= pageFSViewRecheck {
		r.checked = time.Now()
		// The launch is re-read even for the same conversation: a sandbox
		// unlock, restore or assign resumes it under another posture.
		cur, err := db.CurrentConvForAgent(r.agentID)
		if err != nil || cur == "" {
			cur = r.conv
		}
		if next := pageFSViewFor(cur, r.worktree); cur != r.conv || !next.sameAs(r.view) {
			r.conv, r.view = cur, next
			reload = r.changedLocked()
		}
	}
	if r.view.class != pageFSSandboxRoot || (r.root != nil && r.root.Alive()) {
		return reload, nil
	}
	r.dropRootLocked()
	root, err := pageFSOpenSandboxRoot(r.agentID)
	if err != nil {
		return reload, err
	}
	dir := -1
	for _, p := range r.worktree {
		if dir, err = root.OpenDir(p); err == nil {
			break
		}
	}
	if dir < 0 {
		root.Close()
		return reload, errors.New("the agent's sandbox does not show its worktree")
	}
	if r.lastNS != "" && r.lastNS != root.NS() {
		// Another sandbox (a relaunch): what the page shows came from the
		// old one.
		reload = r.changedLocked()
	}
	r.root, r.dir, r.lastNS = root, dir, root.NS()
	return reload, nil
}

func (r *pageFSViewer) refresh(force bool) (*pageFSView, uint64, int, error) {
	r.mu.Lock()
	reload, err := r.refreshLocked(force)
	v, gen := r.view, r.gen
	dir := -1
	if err == nil && v.class == pageFSSandboxRoot {
		dir, err = unix.Dup(r.dir)
		if err == nil {
			unix.CloseOnExec(dir)
		}
	}
	r.mu.Unlock()
	if reload != nil {
		reload()
	}
	return v, gen, dir, err
}

// hidden is the stage's check for one canonical path.
// It uses the view as last refreshed: every walk refreshes it first (base).
func (r *pageFSViewer) hidden(abs string) bool {
	r.mu.Lock()
	v := r.view
	r.mu.Unlock()
	return v.hidden(abs)
}

// base is where the stage walks the worktree from: the agent's own view of
// it (a directory fd the caller closes), or -1 for the daemon's view. An
// error means the agent's view is required but unreachable.
func (r *pageFSViewer) base() (fd int, gen uint64, err error) {
	_, gen, fd, err = r.refresh(false)
	if err != nil {
		return -1, gen, err
	}
	return fd, gen, nil
}

// served records that a page document went out under gen.
func (r *pageFSViewer) served(gen uint64) {
	r.mu.Lock()
	if gen == r.gen {
		r.shown = gen
	}
	r.mu.Unlock()
}

// readBack says whether the page's content may go back to the agent. live
// asks about what the operator's browser shows now, which must have been
// served under the current view; a snapshot renders afresh and need not.
func (r *pageFSViewer) readBack(live bool) bool {
	// The agent's generation is read now, not as last rechecked: a launch
	// that has just become confined must not be read back as before.
	v, gen, dir, err := r.refresh(true)
	if dir >= 0 {
		_ = unix.Close(dir)
	}
	if err != nil || !v.readBack() {
		return false
	}
	if !live {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.shown == gen && r.gen == gen
}

// close releases the held sandbox root.
func (r *pageFSViewer) close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	r.dropRootLocked()
	r.mu.Unlock()
}
