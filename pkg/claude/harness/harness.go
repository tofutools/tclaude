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

// SupportsConvs reports whether the harness exposes a ConvStore. Callers
// that fall back to ConvStore (e.g. a rename for a harness without an
// in-pane rename command) must check this first — a descriptor may leave
// Convs nil.
func (h *Harness) SupportsConvs() bool {
	return h != nil && h.Convs != nil
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
