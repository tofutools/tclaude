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

	// spawnEfforts / spawnModels record the effort and model strings
	// each simSpawner.SpawnNew received, keyed by the new conv-id, so a
	// flow test can assert what the spawn path threaded end-to-end. The
	// unset case ("") is recorded too. Guarded by spawnMu — spawns are
	// sequential in flow tests, but the post-init goroutines make the
	// mutex cheap insurance.
	spawnMu      sync.Mutex
	spawnEfforts map[string]string
	spawnModels  map[string]string
}

// New builds a World wired to a fresh tmpdir HOME, a clean test DB,
// and an empty TmuxSim/CCRegistry pair ready to be plugged into the
// production boundaries (clcommon.Default and agentd.Spawn).
//
// The harness does NOT install the package-var swaps itself; the
// test owns that so it can use t.Cleanup to restore. See
// flow_setup_test.go in package agentd_test for the canonical
// wiring, and DefaultMocks below for the mock impls.
func New(t *testing.T) *World {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	db.ResetForTest()
	return &World{
		HomeDir:      home,
		Tmux:         newTmuxSim(),
		CCs:          newCCRegistry(),
		spawnEfforts: map[string]string{},
		spawnModels:  map[string]string{},
	}
}

// RecordSpawnEffort captures the effort a simSpawner.SpawnNew received,
// keyed by the new conv-id, so a flow test can assert what effort the
// spawn path threaded through. The unset case ("") is recorded too.
func (w *World) RecordSpawnEffort(convID, effort string) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	w.spawnEfforts[convID] = effort
}

// SpawnEffort returns the effort recorded for a spawned conv-id and
// whether a spawn for that conv was observed.
func (w *World) SpawnEffort(convID string) (string, bool) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	e, ok := w.spawnEfforts[convID]
	return e, ok
}

// RecordSpawnModel captures the model a simSpawner.SpawnNew received,
// keyed by the new conv-id, so a flow test can assert what model the
// spawn path threaded through. The unset case ("") is recorded too.
func (w *World) RecordSpawnModel(convID, model string) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	w.spawnModels[convID] = model
}

// SpawnModel returns the model recorded for a spawned conv-id and
// whether a spawn for that conv was observed.
func (w *World) SpawnModel(convID string) (string, bool) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	m, ok := w.spawnModels[convID]
	return m, ok
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
