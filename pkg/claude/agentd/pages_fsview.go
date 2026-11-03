package agentd

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// An agent's view of the file system, for static live pages (tcl-kxiqqd).
//
// The stage opens a static source with the daemon's view of the file system.
// A sandboxed agent may see less: its profile can hide paths inside its own
// worktree (a deny rule, a tmpfs overlay), project another directory onto one
// (a remapped mount), or build a root with only some paths bound. A static
// page must not serve what the agent itself cannot read, because what a page
// serves can come back to the agent (`page marks`, `page snapshot`, `page
// dom`). So the stage serves a path only when the agent's recorded sandbox
// plan shows that path as the host's own file.
//
// The plan is the launch's recorded effective profile (the session row's
// EffectiveSandbox), rendered as tclaude's sandbox renders it. Every
// implementation starts from that profile, so its hidden paths are the
// operator's intent whichever boundary enforces them; reading a
// harness-builtin launch's plan this way can only hide more, never less. The
// protected roots and the tmux socket directory are hidden for every agent.
//
// A sandboxed launch without a recorded profile has an unknown view: its
// pages are served as before, but nothing of them is read back.

const pageFSViewRecheck = 5 * time.Second

// pageFSView is one generation's view.
type pageFSView struct {
	// known says the view is the agent's: an unconfined agent, or a sandboxed
	// one with a recorded plan. An unknown view hides nothing (the page is
	// served as it always was) and is not read back.
	known bool
	// constructed says paths no entry binds are absent (a constructed root).
	constructed bool
	entries     []pageFSEntry
	// always are hidden whatever the entries say, in a known view or not.
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

func pageFSUnder(p, root string) bool {
	if root == "/" {
		return true
	}
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// hidden reports whether the agent cannot read abs (a canonical host path) as
// the host's own file. The last entry that covers abs decides, as the later
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
	if !v.known {
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

// pageFSViewFor builds the view of the session behind convID.
func pageFSViewFor(convID string) *pageFSView {
	v := &pageFSView{}
	protected, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return v // unknown: nothing is read back
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
	if implementation.OmitsOSConfinement() {
		v.known = true
		return v
	}
	if row.EffectiveSandbox == nil {
		return v
	}
	plan, err := sandboxpolicy.RenderMountPlan(row.EffectiveSandbox.Effective)
	if err != nil {
		return v
	}
	v.known = true
	v.constructed = plan.EffectiveRootPosture() == sandboxpolicy.RootConstructed
	for _, e := range plan.Entries {
		hidden := e.Mode == sandboxpolicy.MountHide || e.Mode == sandboxpolicy.MountTmpfs || e.IsRemapped()
		v.entries = append(v.entries, pageFSEntry{paths: pageFSSpellings(e.Path), hidden: hidden})
	}
	return v
}

// pageFSViewer follows an agent's current generation: a reincarnation with
// another profile gets its own view.
type pageFSViewer struct {
	agentID string

	mu      sync.Mutex
	conv    string
	view    *pageFSView
	checked time.Time
}

func newPageFSViewer(agentID, convID string) *pageFSViewer {
	return &pageFSViewer{agentID: agentID, conv: convID, view: pageFSViewFor(convID), checked: time.Now()}
}

// current is the view of the agent's current generation, rechecked every
// few seconds.
func (r *pageFSViewer) current() *pageFSView {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.checked) >= pageFSViewRecheck {
		r.checked = time.Now()
		if cur, err := db.CurrentConvForAgent(r.agentID); err == nil && cur != "" && cur != r.conv {
			r.conv = cur
			r.view = pageFSViewFor(cur)
		}
	}
	return r.view
}

// hidden is the stage's check for one canonical path.
func (r *pageFSViewer) hidden(abs string) bool { return r.current().hidden(abs) }

// known says the page's content may be read back to the agent.
func (r *pageFSViewer) known() bool {
	v := r.current()
	return v != nil && v.known
}
