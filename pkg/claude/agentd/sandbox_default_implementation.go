package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The install default is the lowest tier of the sandbox-implementation chain:
// explicit > named profile > group default profile > global default profile >
// INSTALL DEFAULT > harness default. See db.SandboxDefaultImplementationPrefKey
// for its three states.
//
// It is a tier, not a rewrite of what "unset" means. Unset keeps carrying
// "each harness's historical behavior" everywhere it is already relied on
// (OpenCode's command filter, reassignment, relaunch records), and an install
// that never decided resolves exactly as it did before this tier existed.

// sandboxDefaultImplementation is the seam flow tests swap. It fails open to
// undecided: a broken preference row must not make every spawn fail.
var sandboxDefaultImplementation = func() string {
	value, _, err := db.GetSandboxDefaultImplementation()
	if err != nil {
		slog.Warn("agentd: unreadable default sandbox implementation; treating as undecided", "error", err)
		return ""
	}
	return value
}

// resolveSandboxImplementationLaunchField resolves the sandbox implementation
// through the profile tiers and then, only when none of them set it, the
// install default. Every resolution site goes through here so the dashboard's
// launch-defaults preview, the HTTP spawn, the spawn safety net and template
// deploys cannot disagree about which tier won.
func resolveSandboxImplementationLaunchField(
	h *harness.Harness, explicitValue string, tiers []launchProfileTier,
) (value, source, note string, fail *spawnFailure) {
	value, source, note, fail = resolveStringLaunchField(
		sandboxImplementationField, explicitValue, h.Name, tiers,
		func(p *db.SpawnProfile) string { return p.SandboxImplementation },
		func(raw string) (string, error) { return validateSandboxImplementationForHarness(h, raw) })
	if fail != nil || source != agent.ProvHarnessDefault {
		return value, source, note, fail
	}
	if sandboxDefaultImplementation() != db.SandboxDefaultTclaudeLayer {
		return value, source, note, nil
	}
	// Ambient configuration, so a harness that cannot host the layer falls
	// through to its own default with a disclosure, like a foreign-harness
	// profile tier does. Every registered harness can host it today; this is
	// what keeps a future one from being refused by a default nobody typed.
	if err := session.ValidateTclaudeLayerHarness(h.Name); err != nil {
		skip := fmt.Sprintf("%s %s ignored for %s: %v",
			agent.ProvInstallDefault, sandboxImplementationField, h.Name, err)
		if note != "" {
			skip = note + "; " + skip
		}
		return value, source, skip, nil
	}
	return string(sandboxpolicy.ImplementationTclaudeLayer), agent.ProvInstallDefault, note, nil
}

// withInstallDefaultRemedy names the two ways out when the install default is
// what put tclaude's sandbox on a launch this host cannot run. The refusal
// itself stands — a default the operator chose must not silently downgrade —
// but nobody typed this value at this launch, so the message must say where it
// came from.
func withInstallDefaultRemedy(fail *spawnFailure, source string) *spawnFailure {
	if fail == nil || source != agent.ProvInstallDefault {
		return fail
	}
	out := *fail
	out.Msg += fmt.Sprintf(
		" (tclaude's sandbox is this install's default; install the missing tooling — the dashboard's Diagnostics › Doctor view lists it — "+
			"or run `tclaude agent sandbox-impl default set %s` to keep each harness's own sandbox)",
		db.SandboxDefaultHarness)
	return &out
}

type sandboxDefaultImplementationJSON struct {
	// Value is tclaude-layer or harness; empty while undecided.
	Value   string `json:"value"`
	Decided bool   `json:"decided"`
}

func currentSandboxDefaultImplementation() (sandboxDefaultImplementationJSON, error) {
	value, decided, err := db.GetSandboxDefaultImplementation()
	return sandboxDefaultImplementationJSON{Value: value, Decided: decided}, err
}

// handleSandboxDefaultImplementation serves GET/PUT/DELETE
// /v1/sandbox-impl-default (and the dashboard's /api twin). Reads are ungated —
// like the spawn-launch-defaults preview, they only report which tier a launch
// would use. Writes change how every future agent is confined, so they take
// the same human-only slug as the global sandbox-profile assignment.
func handleSandboxDefaultImplementation(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
	case http.MethodPut:
		if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
			return
		}
		var body struct {
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		if _, err := db.NormalizeSandboxDefaultImplementation(body.Value); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		if _, err := db.SetSandboxDefaultImplementation(strings.TrimSpace(body.Value)); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
	case http.MethodDelete:
		if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
			return
		}
		if err := db.ClearSandboxDefaultImplementation(); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, PUT or DELETE")
		return
	}
	if r.Method != http.MethodGet {
		invalidateDoctorChecks()
	}
	resp, err := currentSandboxDefaultImplementation()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
