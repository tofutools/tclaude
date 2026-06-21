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
	// SkipSpawnRow, when true, makes the CC simSpawner build the pane (CCSim +
	// .jsonl + tmux registration) but NOT write the SessionRow — modelling a
	// forked `tclaude session new` whose row write lags past the daemon's
	// conv-id poll on a slow host. It lets a flow test exercise the
	// launch-enrollment timeout branch (the daemon must return success against
	// the preset id and keep the enrollment, never roll back a live pane).
	SkipSpawnRow bool

	// Codexes is the Codex analog of CCs: conv-id → CodexSim, so the
	// simSpawner's `--harness codex` branch can stash the sim it built and
	// the resume branch can re-attach it. Kept as a parallel registry (not
	// unified with CCs) because the two sims expose harness-specific
	// surfaces — CCs.Clear pokes CCSim's /clear rotation, CodexSim has no
	// such concept — and a typed store keeps each test reaching for the
	// right one without a cast.
	Codexes *CodexRegistry

	// spawnEfforts / spawnModels record the effort and model strings
	// each simSpawner.SpawnNew received, keyed by the new conv-id, so a
	// flow test can assert what the spawn path threaded end-to-end. The
	// unset case ("") is recorded too. Guarded by spawnMu — spawns are
	// sequential in flow tests, but the post-init goroutines make the
	// mutex cheap insurance.
	spawnMu            sync.Mutex
	spawnEfforts       map[string]string
	spawnModels        map[string]string
	spawnSandboxes     map[string]string
	spawnApprovals     map[string]string
	spawnAutoReview    map[string]bool
	spawnTrustDir      map[string]bool
	spawnRemoteControl map[string]bool
	// spawnNames / spawnInitialPrompts record the launch-arg display name
	// (`--name`) and first-turn prompt (`--initial-prompt`) the launch-
	// enrollment spawn path threaded through, keyed by the new conv-id, so a
	// flow test can assert the agent was named + greeted at launch rather than
	// via post-connect tmux injection.
	spawnNames          map[string]string
	spawnInitialPrompts map[string]string
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
		HomeDir:             home,
		Tmux:                newTmuxSim(),
		CCs:                 newCCRegistry(),
		Codexes:             newCodexRegistry(),
		spawnEfforts:        map[string]string{},
		spawnModels:         map[string]string{},
		spawnSandboxes:      map[string]string{},
		spawnApprovals:      map[string]string{},
		spawnAutoReview:     map[string]bool{},
		spawnTrustDir:       map[string]bool{},
		spawnRemoteControl:  map[string]bool{},
		spawnNames:          map[string]string{},
		spawnInitialPrompts: map[string]string{},
	}
}

// RecordSpawnName captures the launch-arg display name (`claude --name`) a
// simSpawner.SpawnNew received, keyed by the new conv-id. "" (no launch name)
// is recorded too.
func (w *World) RecordSpawnName(convID, name string) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	w.spawnNames[convID] = name
}

// SpawnName returns the launch name recorded for a spawned conv-id and whether
// a spawn for that conv was observed.
func (w *World) SpawnName(convID string) (string, bool) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	n, ok := w.spawnNames[convID]
	return n, ok
}

// RecordSpawnInitialPrompt captures the launch-arg first-turn prompt
// (`claude <prompt>`) a simSpawner.SpawnNew received, keyed by the new conv-id.
// "" (no launch prompt) is recorded too.
func (w *World) RecordSpawnInitialPrompt(convID, prompt string) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	w.spawnInitialPrompts[convID] = prompt
}

// SpawnInitialPrompt returns the launch prompt recorded for a spawned conv-id
// and whether a spawn for that conv was observed.
func (w *World) SpawnInitialPrompt(convID string) (string, bool) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	p, ok := w.spawnInitialPrompts[convID]
	return p, ok
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

// RecordSpawnSandbox captures the sandbox mode a simSpawner.SpawnNew /
// SpawnResume received, keyed by the new conv-id, so a flow test can assert
// the sandbox flag the spawn path threaded through (JOH-192). The unset
// case ("") is recorded too.
func (w *World) RecordSpawnSandbox(convID, sandbox string) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	w.spawnSandboxes[convID] = sandbox
}

// SpawnSandbox returns the sandbox mode recorded for a spawned conv-id and
// whether a spawn for that conv was observed.
func (w *World) SpawnSandbox(convID string) (string, bool) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	s, ok := w.spawnSandboxes[convID]
	return s, ok
}

// RecordSpawnApproval captures the approval policy a simSpawner.SpawnNew /
// SpawnResume received, keyed by the new conv-id, so a flow test can assert
// the --ask-for-approval flag the spawn path threaded through (JOH-200). The
// unset case ("") is recorded too.
func (w *World) RecordSpawnApproval(convID, approval string) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	w.spawnApprovals[convID] = approval
}

// SpawnApproval returns the approval policy recorded for a spawned conv-id and
// whether a spawn for that conv was observed.
func (w *World) SpawnApproval(convID string) (string, bool) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	a, ok := w.spawnApprovals[convID]
	return a, ok
}

// RecordSpawnAutoReview captures the auto-review (guardian) opt-in a
// simSpawner.SpawnNew / SpawnResume received, keyed by the new conv-id, so a
// flow test can assert the `--auto-review` flag the spawn path threaded through
// (JOH-200 part 2). The default (false) is recorded too.
func (w *World) RecordSpawnAutoReview(convID string, autoReview bool) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	w.spawnAutoReview[convID] = autoReview
}

// SpawnAutoReview returns the auto-review opt-in recorded for a spawned conv-id
// and whether a spawn for that conv was observed.
func (w *World) SpawnAutoReview(convID string) (bool, bool) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	a, ok := w.spawnAutoReview[convID]
	return a, ok
}

// RecordSpawnTrustDir captures the opt-in dir-trust flag a simSpawner.SpawnNew
// received, keyed by the new conv-id, so a flow test can assert the
// `--trust-dir` the spawn path threaded through (JOH-205 inc4). The default
// (false) is recorded too. The sim runs in-process, so the actual
// ~/.codex/config.toml write does NOT happen here — that is covered by the
// harness package's editor unit tests; this only proves the flag's plumbing.
func (w *World) RecordSpawnTrustDir(convID string, trustDir bool) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	w.spawnTrustDir[convID] = trustDir
}

// SpawnTrustDir returns the dir-trust opt-in recorded for a spawned conv-id
// and whether a spawn for that conv was observed.
func (w *World) SpawnTrustDir(convID string) (bool, bool) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	td, ok := w.spawnTrustDir[convID]
	return td, ok
}

// RecordSpawnRemoteControl captures the remote-control opt-in a
// simSpawner.SpawnNew / SpawnResume received, keyed by the new conv-id, so a
// flow test can assert the `--remote-control` the spawn path threaded through.
// The default (false) is recorded too. Recorded on the fresh-spawn path
// (JOH-258) AND the resume path (JOH-261, re-arming Remote Access across
// resume/reincarnate/clone).
func (w *World) RecordSpawnRemoteControl(convID string, remoteControl bool) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	w.spawnRemoteControl[convID] = remoteControl
}

// SpawnRemoteControl returns the remote-control opt-in recorded for a spawned
// conv-id and whether a spawn for that conv was observed.
func (w *World) SpawnRemoteControl(convID string) (bool, bool) {
	w.spawnMu.Lock()
	defer w.spawnMu.Unlock()
	rc, ok := w.spawnRemoteControl[convID]
	return rc, ok
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

// CodexRegistry is the Codex analog of CCRegistry: conv-id → CodexSim,
// multi-keyed by label too. The simSpawner's `--harness codex` branch
// records the sim it built here so a later resume re-attaches the same
// instance instead of synthesising a new one.
type CodexRegistry struct {
	mu       sync.Mutex
	byConvID map[string]*CodexSim
	byLabel  map[string]*CodexSim
}

func newCodexRegistry() *CodexRegistry {
	return &CodexRegistry{
		byConvID: map[string]*CodexSim{},
		byLabel:  map[string]*CodexSim{},
	}
}

// Set records a CodexSim under both label and conv-id (label may be
// empty for hydrated sims). A later SetByConvID with the same id
// overwrites — useful when a resume creates a new label for an existing
// conv.
func (r *CodexRegistry) Set(label string, cx *CodexSim) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if label != "" {
		r.byLabel[label] = cx
	}
	r.byConvID[cx.ConvID] = cx
}

// SetByConvID registers a sim by conv-id only. Used for hydrate-from-
// disk scenarios where the original label is unknown.
func (r *CodexRegistry) SetByConvID(cx *CodexSim) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byConvID[cx.ConvID] = cx
}

// GetByConvID returns the registered sim for convID, or nil.
func (r *CodexRegistry) GetByConvID(convID string) *CodexSim {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byConvID[convID]
}

// GetByLabel returns the registered sim for label, or nil.
func (r *CodexRegistry) GetByLabel(label string) *CodexSim {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byLabel[label]
}
