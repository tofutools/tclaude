package agentd

import (
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// spawnEffectiveSandboxJSON is the answer to "if I spawned with these settings
// right now, would anything actually confine the agent?".
//
// The dashboard cannot compute this itself. The sandbox mode a spawn dialog
// shows is usually `inherit`, which means "whatever the operator's settings.json
// says" — a fact that lives in files on the daemon's host, not in the snapshot.
// So the browser asks, rather than guessing from the mode token and getting the
// common case exactly backwards.
type spawnEffectiveSandboxJSON struct {
	// Harness / SandboxMode / Approval echo the values the check actually ran
	// on, AFTER the harness defaults were applied. The dialog can leave either
	// select on its blank "default" option, and the echo is how the operator
	// sees which posture that resolved to.
	Harness     string `json:"harness"`
	SandboxMode string `json:"sandbox_mode"`
	Approval    string `json:"approval"`
	// SandboxState is "on", "off", or "unconfigured" — see
	// harness.ClaudeSandboxState. SandboxSource names whatever decided it (the
	// launch itself, or the settings file that won the precedence chain), and is
	// empty when nothing did.
	SandboxState  string `json:"sandbox_state"`
	SandboxSource string `json:"sandbox_source"`
	// Warnings is the same operator-facing copy the CLI prints, so the dialog
	// and `tclaude agent spawn` cannot describe one situation two ways. Empty
	// means the pairing is sound (or does not apply to this harness).
	Warnings []string `json:"warnings"`
}

// handleDashboardSpawnEffectiveSandbox serves
// GET /api/spawn/effective-sandbox?harness=&sandbox=&approval=&dir= — the probe
// behind the spawn dialog's unsandboxed-autonomy warning (TCL-586).
//
// The loopback popup server has no global auth middleware — each handler
// pins itself on the dashboard cookie + Origin, exactly like its neighbours
// (handleDashboardClaudeDefaultModel, handleDashboardCostFactorAPI). This one
// must too: it drives filesystem reads off the caller-supplied `dir`, and its
// verdict is a security-relevant signal, so it is not left ungated.
//
// Beyond that pin it is read-only: it reveals only whether a sandbox is enabled
// and which settings file said so, never any other settings content. It
// deliberately resolves through the same Resolve* helpers the daemon spawn
// boundary uses, so a blank select answers for the default the spawn would
// really get instead of for "nothing chosen".
//
// An unknown harness or an invalid mode is a 400, matching the spawn endpoint
// that would reject the same values.
func handleDashboardSpawnEffectiveSandbox(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	query := r.URL.Query()
	h, err := resolveSpawnHarness(strings.TrimSpace(query.Get("harness")))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_harness", err.Error())
		return
	}
	sandboxMode, err := harness.ResolveSandboxMode(h, query.Get("sandbox"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox", err.Error())
		return
	}
	approval, err := harness.ResolveApprovalPolicy(h, query.Get("approval"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_approval", err.Error())
		return
	}
	// The dialog's CWD field accepts `~/…`, and an unexpanded tilde would make
	// the project-settings walk find nothing — a silent all-clear, the one
	// failure mode this endpoint must not have.
	cwd := expandTilde(strings.TrimSpace(query.Get("dir")))
	// SandboxState/SandboxSource describe Claude Code's settings.json OS sandbox
	// specifically. Resolving them for another harness would walk Claude's
	// settings for an agent that never reads them and could echo `on` from the
	// operator's own ~/.claude config while the warnings below correctly say the
	// OpenCode agent is unsandboxed. Leave them at the zero resolution
	// (unconfigured / no source) off the Claude path. h.Name is the registry's
	// canonical harness id, so the direct compare is exact.
	var resolution harness.ClaudeSandboxResolution
	if h.Name == harness.DefaultName {
		resolution = harness.ResolveClaudeSandboxEnabled(sandboxMode, cwd)
	}
	warnings := harness.SpawnSandboxWarnings(h, approval, sandboxMode, cwd)
	if warnings == nil {
		// A JSON null here would make every consumer guard the array.
		warnings = []string{}
	}
	writeJSON(w, http.StatusOK, spawnEffectiveSandboxJSON{
		Harness:       h.Name,
		SandboxMode:   sandboxMode,
		Approval:      approval,
		SandboxState:  resolution.State.String(),
		SandboxSource: resolution.Source,
		Warnings:      warnings,
	})
}
