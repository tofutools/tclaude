package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/launchspec"
)

// The relaunch policy (launch slice 6d onward): one global answer per
// configurable launch field, stored in relaunch_policy and read on every
// relaunch. A field with no stored answer takes its default, so nothing
// configured is the default policy. Reads are open; changing it takes
// relaunch-policy.manage, since an answer can widen what a relaunch gives
// every agent.

// currentRelaunchPolicy is the stored policy. A row that no longer validates
// (a field or mode a newer build wrote, say) is skipped and logged, so its
// field takes its default. A read error must not quietly undo a security
// setting, so it gives tighten-only for harness_config: an explicit read pin
// still stays, and only an explicit write over a floored chain is narrowed.
func currentRelaunchPolicy() launchspec.RelaunchPolicy {
	rows, err := db.ListRelaunchPolicyRows()
	if err != nil {
		slog.Warn("relaunch policy: read failed; harness_config falls back to tighten-only", "error", err)
		return launchspec.RelaunchPolicy{Rows: []launchspec.PolicyRow{
			{Scope: launchspec.ScopeGlobal, Field: harnessConfigField, Mode: launchspec.ModeTightenOnly}}}
	}
	var p launchspec.RelaunchPolicy
	for _, row := range rows {
		candidate := launchspec.PolicyRow{Scope: row.Scope, Field: row.Field, Mode: launchspec.RelaunchMode(row.Mode)}
		if err := (launchspec.RelaunchPolicy{Rows: []launchspec.PolicyRow{candidate}}).Validate(); err != nil {
			slog.Warn("relaunch policy: ignoring a stored row", "field", row.Field, "mode", row.Mode, "error", err)
			continue
		}
		p.Rows = append(p.Rows, candidate)
	}
	return p
}

const harnessConfigField = "harness_config"

// relaunchPolicyRead are the configurable rows a relaunch reads today. The
// others are listed but refuse a stored answer until their slice wires them,
// so no setting is accepted that would silently do nothing.
var relaunchPolicyRead = map[string]bool{harnessConfigField: true}

// harnessConfigNarrower reports whether the chain's posture is narrower than
// the explicit one: only an explicit write over a chain that keeps the
// read-only floor. An explicit read pin is already the strictest posture.
func harnessConfigNarrower(chain, explicit sandboxpolicy.HarnessConfigAccess) bool {
	return !sandboxpolicy.HarnessConfigFloorApplies(explicit) && sandboxpolicy.HarnessConfigFloorApplies(chain)
}

// relaunchHarnessConfig decides the posture a relaunch keeps, given the
// explicit posture and the chain's: under explicit-wins the explicit posture
// stays; under tighten-only it stays unless the chain's is narrower; under
// chain-wins the chain's applies. It returns the explicit posture to apply,
// "" to keep the chain's.
func relaunchHarnessConfig(mode launchspec.RelaunchMode, chain sandboxpolicy.HarnessConfigAccess, explicit string) string {
	if explicit == "" {
		return ""
	}
	switch mode {
	case launchspec.ModeChainWins:
		return ""
	case launchspec.ModeTightenOnly:
		if harnessConfigNarrower(chain, sandboxpolicy.HarnessConfigAccess(explicit)) {
			return ""
		}
	}
	return explicit
}

// explicitHarnessConfigFor is an agent's explicit --harness-config posture: the
// one its last launch recorded (explicitHarnessConfig), else the one its
// spawn request set (launch_layers' Explicit row, captured once at spawn).
// The second covers a launch the policy gave the chain's posture, which
// records none, so switching the policy back restores the explicit one.
func explicitHarnessConfigFor(conv string, previous *sandboxpolicy.Snapshot) string {
	if explicit := explicitHarnessConfig(previous); explicit != "" || previous == nil || previous.ProfilesOmitted {
		return explicit
	}
	if conv == "" {
		return ""
	}
	layers, err := db.AgentLaunchLayersForConv(conv)
	if err != nil || layers == nil {
		return ""
	}
	raw, ok := layers.Explicit[harnessConfigField].Fields["HarnessConfig"]
	if !ok {
		return ""
	}
	var explicit string
	if json.Unmarshal(raw, &explicit) != nil {
		return ""
	}
	return strings.TrimSpace(explicit)
}

type relaunchPolicyRowJSON struct {
	Field   string   `json:"field"`
	Label   string   `json:"label"`
	Mode    string   `json:"mode"`
	Default string   `json:"default"`
	Modes   []string `json:"modes"`
	Set     bool     `json:"set"`
	// Settable is false for a row relaunches do not read yet.
	Settable bool   `json:"settable"`
	Why      string `json:"why"`
	Gap      string `json:"gap,omitempty"`
}

// relaunchPolicyView is every configurable row with its effective mode.
func relaunchPolicyView() []relaunchPolicyRowJSON {
	p := currentRelaunchPolicy()
	set := map[string]bool{}
	for _, r := range p.Rows {
		set[r.Field] = true
	}
	out := []relaunchPolicyRowJSON{}
	for _, f := range launchspec.Fields {
		modes := f.RelaunchModes()
		if len(modes) == 0 {
			continue
		}
		names := make([]string, len(modes))
		for i, m := range modes {
			names[i] = string(m)
		}
		out = append(out, relaunchPolicyRowJSON{
			Field: f.Key, Label: f.Label, Mode: string(p.Mode(f.Key)), Default: names[0],
			Modes: names, Set: set[f.Key], Settable: relaunchPolicyRead[f.Key], Why: f.Why, Gap: f.Gap,
		})
	}
	return out
}

// serveRelaunchPolicy is GET (open) and PUT {field, mode} (relaunch-policy.manage)
// of /v1/relaunch-policy. An empty mode, or the field's default, clears the
// stored row.
func serveRelaunchPolicy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"scope": launchspec.ScopeGlobal, "rows": relaunchPolicyView()})
	case http.MethodPut:
		caller, ok := requirePermission(w, r, PermRelaunchPolicyManage)
		if !ok {
			return
		}
		var body struct {
			Field string `json:"field"`
			Mode  string `json:"mode"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", `body must be {"field": "...", "mode": "..."}`)
			return
		}
		field, mode := strings.TrimSpace(body.Field), strings.TrimSpace(body.Mode)
		f, known := launchspec.Lookup(field)
		if !known || len(f.RelaunchModes()) == 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("%q is not a configurable relaunch field", field))
			return
		}
		if !relaunchPolicyRead[field] {
			writeError(w, http.StatusBadRequest, "not_settable", fmt.Sprintf(
				"%s cannot be set yet: relaunches do not read its policy row, so a stored answer would do nothing", field))
			return
		}
		if mode == "" {
			mode = string(f.RelaunchModes()[0])
		}
		row := launchspec.PolicyRow{Scope: launchspec.ScopeGlobal, Field: field, Mode: launchspec.RelaunchMode(mode)}
		if err := (launchspec.RelaunchPolicy{Rows: []launchspec.PolicyRow{row}}).Validate(); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		before := string(currentRelaunchPolicy().Mode(field))
		var err error
		if mode == string(f.RelaunchModes()[0]) {
			_, err = db.ClearRelaunchPolicyRow(launchspec.ScopeGlobal, field)
		} else {
			err = db.SetRelaunchPolicyRow(launchspec.ScopeGlobal, field, mode, granterLabel(caller))
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		setAuditTargetLabel(r, fmt.Sprintf("relaunch policy %s: %s → %s", field, before, mode))
		writeJSON(w, http.StatusOK, map[string]any{"scope": launchspec.ScopeGlobal, "rows": relaunchPolicyView()})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or PUT")
	}
}
