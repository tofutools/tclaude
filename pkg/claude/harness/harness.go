// Package harness defines the seam that lets tclaude drive more than one
// coding harness. Claude Code (CC) is the harness today; OpenAI Codex CLI
// is next (project tclaude-harness-independence, JOH-149…163).
//
// The seam is deliberately NOT one monolithic `Harness` interface. The
// same feature is distributed differently across each harness's
// components — e.g. a session rename is one logical contract, but CC
// implements it by injecting `/rename` (which writes a `customTitle` turn
// to the conversation `.jsonl`) while Codex implements it against its own
// title store. So we model focused, capability-segregated contracts
// (spawn a session, validate a model, name the lifecycle slash commands)
// and let each harness satisfy each one however its storage/command model
// dictates. A small Harness descriptor composes the pieces and exposes
// capability flags for features a harness lacks.
//
// PR scope (JOH-150, first slice): the descriptor + registry, the
// Spawner, ModelCatalog and Lifecycle contracts, and the `claude`
// implementation registered as the default. The spawn path
// (`tclaude session new`) is routed through Spawner + ModelCatalog here.
// The remaining contracts named in the design doc — ConvStore (assemble a
// SessionEntry from the harness's full storage model), HookInstaller,
// HookEventMap and StatuslineInstaller — are deliberately deferred to
// follow-up slices so this PR stays a clean, reviewable, zero-behavior-
// change refactor of the CC spawn path. The Lifecycle tokens are defined
// and tested here but their slash-injection call sites in agentd are
// rewired in a separate PR (it touches the send-keys injection sink and
// gets its own cold review).
package harness

import (
	"fmt"
	"sort"
	"sync"
)

// DefaultName is the harness tclaude assumes when none is recorded. It
// matches the `harness TEXT NOT NULL DEFAULT 'claude'` column default so
// every existing session/conv row keeps resolving to Claude Code.
const DefaultName = "claude"

// Harness is a descriptor composing the segregated per-harness contracts
// plus capability flags. A nil sub-contract means "this harness does not
// provide that capability"; the Supports* helpers fold those into simple
// booleans for callers that gate behavior on a capability.
type Harness struct {
	// Name is the stable identifier persisted in the DB `harness` column
	// and accepted by `--harness`. Lower-case, no spaces (e.g. "claude").
	Name string
	// DisplayName is the human-facing label (e.g. "Claude Code").
	DisplayName string

	// Spawn builds the in-tmux launch command + resume form.
	Spawn Spawner
	// Ask builds the argv for a one-shot `tclaude ask` turn (a foreground,
	// terminal-attached question/answer against a resumable per-(terminal,cwd)
	// thread). nil = this harness can't answer ad-hoc questions yet; callers
	// gate on SupportsAsk and degrade with a clear message. See JOH-250.
	Ask Asker
	// Models validates/normalizes model + effort for this harness.
	Models ModelCatalog
	// Life names the lifecycle slash commands this harness understands
	// (or reports them unsupported). Every slash injection must be gated
	// on these so no pane is typed a command it can't parse.
	Life Lifecycle
	// Convs assembles conversation metadata from the harness's storage
	// model (list / resolve / read title). Read-only for now; the write
	// counterpart (SetTitle) rides the Lifecycle/send-keys PR.
	Convs ConvStore
	// Hooks installs/checks/repairs the tclaude callback in the harness's
	// config target (+ any trust step). nil = this build can't install
	// hooks for the harness; `tclaude setup` skips it with a message.
	Hooks HookInstaller
	// Sandbox names the launch-time OS-sandbox modes this harness accepts
	// (Codex's --sandbox) and its secure default. nil for harnesses whose
	// sandbox is configured out of band (Claude Code → settings.json), in
	// which case the spawn path passes no sandbox flag and rejects an
	// explicit mode. See JOH-192.
	Sandbox SandboxCatalog
	// Approval names the launch-time approval policies this harness accepts
	// (Codex's --ask-for-approval) and its non-escalating default. nil for
	// harnesses whose approval handling is configured out of band (Claude
	// Code → settings.json), in which case the spawn path passes no approval
	// flag and rejects an explicit policy. The default exists so a daemon-
	// spawned (unattended) pane can't deadlock on an approval prompt no human
	// can answer — see JOH-200.
	Approval ApprovalCatalog

	// TmuxScrollback marks a harness that relies on tmux for scroll-back
	// history rather than rendering its own. The spawn path turns tmux mouse
	// mode on for *that session only* so the wheel scrolls the pane's
	// copy-mode buffer (see WantsTmuxScrollback + session.ConfigureTmuxScrollback).
	//
	// Claude Code leaves this false: it is a full-screen TUI that owns its
	// offscreen rendering and mouse-wheel handling, so tmux mouse mode would
	// fight it. Codex CLI sets it true — its TUI scrolls through the terminal,
	// which without mouse mode means the wheel does nothing in a tmux pane.
	// See JOH-213.
	TmuxScrollback bool

	// LaunchEnrollment marks a harness whose conv-id can be PRESET at launch
	// (SpawnSpec.SessionID → `claude --session-id <uuid>`) AND whose display
	// name + first-turn prompt can ride in as launch args (SpawnSpec.Name →
	// `--name`, SpawnSpec.InitialPrompt → the positional [prompt]). When true,
	// the daemon spawn path can enroll the agent (group membership + inbox
	// briefing) and bake the rename + welcome into the launch command, instead
	// of polling for the conv-id and injecting `/rename` + the welcome over
	// tmux with delays afterwards. Claude Code sets it true; Codex leaves it
	// false (it generates its own conv-id at first turn and renames out of
	// band), so a Codex spawn keeps the inject-after-connect flow.
	LaunchEnrollment bool
}

// SupportsRename reports whether the harness has a usable in-pane rename
// command. Callers must skip the rename injection when this is false.
func (h *Harness) SupportsRename() bool {
	return h != nil && h.Life != nil && h.Life.RenameCommand() != ""
}

// SupportsCompact reports whether the harness has a usable in-pane
// compaction command.
func (h *Harness) SupportsCompact() bool {
	return h != nil && h.Life != nil && h.Life.CompactCommand() != ""
}

// SupportsSoftExit reports whether the harness has a usable in-pane
// soft-exit command (graceful "/exit" rather than killing the tmux pane).
func (h *Harness) SupportsSoftExit() bool {
	return h != nil && h.Life != nil && h.Life.SoftExitCommand() != ""
}

// SupportsAsk reports whether the harness can answer a one-shot `tclaude ask`
// turn (it provides an Asker). `tclaude ask` gates on this and fails with a
// clear message for a harness that hasn't implemented the ask argv yet (Codex
// — its exec/resume shape is a follow-up), rather than building a bogus
// command. See Harness.Ask / JOH-250.
func (h *Harness) SupportsAsk() bool {
	return h != nil && h.Ask != nil
}

// SupportsConvs reports whether the harness exposes a ConvStore. Callers
// that fall back to ConvStore (e.g. a rename for a harness without an
// in-pane rename command) must check this first — a descriptor may leave
// Convs nil.
func (h *Harness) SupportsConvs() bool {
	return h != nil && h.Convs != nil
}

// CanRename reports whether a rename is DELIVERABLE for this harness by
// either mechanism the daemon's deliverRename dispatch uses: an in-pane
// rename slash command (Claude Code's `/rename`), OR an out-of-band
// title-store write (Codex's ConvStore.SetTitle). It mirrors deliverRename
// exactly — if deliverRename would attempt a path, CanRename is true — so
// a spawn/row UI can gate the rename affordance on "will this actually
// work" rather than on the in-pane flag alone.
//
// This is deliberately broader than SupportsRename: Codex has no TUI
// rename command (SupportsRename() == false) yet renames fine through its
// ConvStore, so gating the dashboard's rename control on SupportsRename
// alone would wrongly hide a working feature for Codex agents.
func (h *Harness) CanRename() bool {
	return h.SupportsRename() || h.SupportsConvs()
}

// CanCompact reports whether a context-compaction is deliverable for this
// harness. Unlike rename there is no out-of-band fallback — compaction is
// only ever an in-pane slash command — so this is exactly SupportsCompact.
// A harness without it must have every compaction affordance hidden, and the
// daemon's compact endpoint already refuses it; this is the matching UI-side
// predicate.
func (h *Harness) CanCompact() bool {
	return h.SupportsCompact()
}

// SupportsLaunchEnrollment reports whether the daemon can spawn this harness
// fully enrolled at launch — preset conv-id + launch-arg rename + launch-arg
// welcome — and so skip the post-connect `/rename` + welcome tmux injection.
// See Harness.LaunchEnrollment.
func (h *Harness) SupportsLaunchEnrollment() bool {
	return h != nil && h.LaunchEnrollment && h.Spawn != nil
}

// SupportsSandbox reports whether the harness takes a launch-time sandbox
// mode (Codex's --sandbox). Callers gate sandbox handling on this; a
// harness that leaves Sandbox nil (Claude Code) keeps its out-of-band
// (settings.json) sandbox untouched and rejects an explicit --sandbox.
func (h *Harness) SupportsSandbox() bool {
	return h != nil && h.Sandbox != nil
}

// SupportsApproval reports whether the harness takes a launch-time approval
// policy (Codex's --ask-for-approval). Callers gate approval handling on this;
// a harness that leaves Approval nil (Claude Code) keeps its out-of-band
// (settings.json) approval behaviour untouched and rejects an explicit
// --ask-for-approval. See JOH-200.
func (h *Harness) SupportsApproval() bool {
	return h != nil && h.Approval != nil
}

// SupportsHooks reports whether this build can install tclaude hooks for
// the harness. `tclaude setup` checks it before dispatching to
// Hooks.Install, and skips with a message when false.
func (h *Harness) SupportsHooks() bool {
	return h != nil && h.Hooks != nil
}

// WantsTmuxScrollback reports whether the spawn path should enable tmux
// mouse mode for this harness's panes so the wheel scrolls the scroll-back
// buffer. False for a harness that renders its own scrollback (Claude
// Code); true for one that leans on the terminal/tmux for it (Codex CLI).
// See the TmuxScrollback field and session.ConfigureTmuxScrollback (JOH-213).
func (h *Harness) WantsTmuxScrollback() bool {
	return h != nil && h.TmuxScrollback
}

// registry holds the registered harnesses keyed by Name. Populated from
// each harness file's init() (single-threaded, before main) and read at
// runtime; the mutex guards against any test that registers concurrently.
var (
	registryMu sync.RWMutex
	registry   = map[string]*Harness{}
)

// Register adds a harness to the registry, overwriting any prior
// registration with the same Name. Intended to be called from init().
func Register(h *Harness) {
	if h == nil || h.Name == "" {
		panic("harness: Register requires a non-nil Harness with a Name")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[h.Name] = h
}

// Get returns the registered harness for name, or (nil, false) when none
// is registered under that name.
func Get(name string) (*Harness, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	h, ok := registry[name]
	return h, ok
}

// MustGet returns the registered harness for name, panicking if none is
// registered. Use for names that are guaranteed registered (e.g. the
// built-in DefaultName); use Get for caller-supplied names.
func MustGet(name string) *Harness {
	h, ok := Get(name)
	if !ok {
		panic(fmt.Sprintf("harness: no harness registered under %q", name))
	}
	return h
}

// Default returns the default harness (Claude Code). It is always
// registered by claude.go's init().
func Default() *Harness {
	return MustGet(DefaultName)
}

// SpawnBinaries returns the executable name of every registered harness
// that can be spawned (has a Spawner) — e.g. ["claude", "codex"]. Used by
// the process-tree walk that recognises a hook callback's harness ancestor
// (session.FindClaudePID), so a newly-registered harness is matched
// without editing that walk. Order is unspecified.
func SpawnBinaries() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for _, h := range registry {
		if h.Spawn != nil {
			out = append(out, h.Spawn.Binary())
		}
	}
	return out
}

// Resolve returns the harness for name, falling back to Default when name
// is empty. An unknown non-empty name is an error so a typo surfaces
// rather than silently running Claude Code.
func Resolve(name string) (*Harness, error) {
	if name == "" {
		return Default(), nil
	}
	if h, ok := Get(name); ok {
		return h, nil
	}
	return nil, fmt.Errorf("unknown harness %q: must be one of %v", name, Names())
}

// ResolveSpawnable resolves name like Resolve, but additionally requires
// the harness to be launchable by tclaude: it must carry both a Spawner
// (to build the `session new` command) and a ModelCatalog (to validate the
// requested model/effort). Spawn surfaces — the daemon's group-spawn, the
// `agent spawn` CLI, `--join-group` — use this so an unknown or
// not-yet-spawnable harness fails fast with a clear message instead of a
// silent conv-id-poll timeout once the forked session exits.
func ResolveSpawnable(name string) (*Harness, error) {
	h, err := Resolve(name)
	if err != nil {
		return nil, err
	}
	if h.Spawn == nil || h.Models == nil {
		return nil, fmt.Errorf("harness %q is not spawnable (no spawner/model catalog)", h.Name)
	}
	return h, nil
}

// Names returns the registered harness names in sorted order.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
