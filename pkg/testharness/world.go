//go:build rewire

package testharness

import (
	"sync"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// World is the per-test scaffolding bundle. Construction is via New(t)
// and cleanup is auto-registered through t.Cleanup, so individual
// tests don't need a Close call.
//
// Deliberately *no* http.Handler / agentd reference: the daemon's
// package owns the mux, and importing it from here would create a
// cycle when flow tests in `package agentd_test` import testharness
// back. Instead, http.go provides handler-agnostic Serve / JSONRequest
// helpers that the test wires to its own mux.
type World struct {
	HomeDir string
	Tmux    *TmuxSim
	CCs     *CCRegistry
}

// New builds a World wired to a fresh tmpdir HOME, a clean test DB,
// and an empty TmuxSim/CCRegistry pair ready to be rewired into
// clcommon.TmuxCommand and the agentd spawn boundaries.
//
// The harness does NOT install the rewires itself: rewire's scanner
// walks `_test.go` files for `rewire.Func` calls, so the test must
// own the install. flow_setup_test.go in package agentd_test does
// this; see DefaultMocks for the canonical wiring.
func New(t *testing.T) *World {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	db.ResetForTest()
	return &World{
		HomeDir: home,
		Tmux:    newTmuxSim(),
		CCs:     newCCRegistry(),
	}
}

// CCRegistry maps conv-id → CCSim so the resume mock can locate the
// existing simulator instead of synthesising a new one. Multi-keyed
// by label too, for the rare scenario that knows the label but not
// the conv-id.
type CCRegistry struct {
	mu       sync.Mutex
	byConvID map[string]*CCSim
	byLabel  map[string]*CCSim
}

func newCCRegistry() *CCRegistry {
	return &CCRegistry{
		byConvID: map[string]*CCSim{},
		byLabel:  map[string]*CCSim{},
	}
}

// Set records a CCSim under both label and conv-id (label may be
// empty for hydrated sims). Subsequent SetByConvID with the same id
// overwrites — useful when a resume creates a new label for an
// existing conv.
func (r *CCRegistry) Set(label string, cc *CCSim) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if label != "" {
		r.byLabel[label] = cc
	}
	r.byConvID[cc.ConvID] = cc
}

// SetByConvID registers a sim by conv-id only. Used for hydrate-from-
// disk scenarios where the original label is unknown.
func (r *CCRegistry) SetByConvID(cc *CCSim) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byConvID[cc.ConvID] = cc
}

// GetByConvID returns the registered sim for convID, or nil.
func (r *CCRegistry) GetByConvID(convID string) *CCSim {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byConvID[convID]
}

// GetByLabel returns the registered sim for label, or nil.
func (r *CCRegistry) GetByLabel(label string) *CCSim {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byLabel[label]
}
