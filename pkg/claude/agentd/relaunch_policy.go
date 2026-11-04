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

// relaunchPolicyRead are the configurable rows a relaunch reads today
// (resume and reincarnate; relaunch_policy_apply.go). A configurable row not
// listed here would refuse a stored answer until a relaunch reads it, so no
// setting is accepted that would silently do nothing.
var relaunchPolicyRead = map[string]bool{
	harnessConfigField:          true,
	groupEnvField:               true,
	groupRemoteControlField:     true,
	sandboxProfileContentsField: true,
}

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
	if explicit := explicitHarnessConfig(previous); explicit != "" || previous == nil || previous.ProfilesOmittedByChoice() {
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

// relaunchFieldJSON is one launch-registry row as the Config tab shows it:
// the configurable ones get a control from rows, the rest a read-only pill.
type relaunchFieldJSON struct {
	Key          string `json:"key"`
	Label        string `json:"label"`
	Layer        string `json:"layer"`
	Relaunch     string `json:"relaunch"`
	Configurable bool   `json:"configurable"`
	Why          string `json:"why"`
	Gap          string `json:"gap,omitempty"`
}

func relaunchFieldsView() []relaunchFieldJSON {
	out := make([]relaunchFieldJSON, 0, len(launchspec.Fields))
	for _, f := range launchspec.Fields {
		out = append(out, relaunchFieldJSON{Key: f.Key, Label: f.Label, Layer: string(f.Layer),
			Relaunch: f.Layer.Relaunch(), Configurable: f.Configurable, Why: f.Why, Gap: f.Gap})
	}
	return out
}

// relaunchPolicyFromPeer reports whether the request came from another
// node's operator through federation (the peer view, a peer grant or a peer
// action). Those mark the request remote-authed with a bare true; this
// node's own remote-access listener stores a session instead.
func relaunchPolicyFromPeer(r *http.Request) bool {
	peer, _ := r.Context().Value(remoteAuthedCtxKey{}).(bool)
	return peer || peerGrantFromRequest(r) != nil || peerActionFromRequest(r) != nil
}

// relaunchPolicyCanManage reports whether the caller could change the policy,
// so the Config tab shows the table read-only when it could not. A peer
// node's operator never can: the policy is this node's own (the peer view is
// read-only, as for an unrestricted peer too).
func relaunchPolicyCanManage(r *http.Request) bool {
	if relaunchPolicyFromPeer(r) {
		return false
	}
	if callerIsHuman(r) {
		return true
	}
	p := peerFromContext(r.Context())
	return p != nil && p.ConvID != "" && standingPermissionAllows(p.ConvID, PermRelaunchPolicyManage, ActionContext{})
}

func relaunchPolicyResponse(r *http.Request) map[string]any {
	return map[string]any{"scope": launchspec.ScopeGlobal, "rows": relaunchPolicyView(),
		"fields": relaunchFieldsView(), "can_manage": relaunchPolicyCanManage(r)}
}

// serveRelaunchPolicy is GET (open) and PUT {field, mode} (relaunch-policy.manage)
// of /v1/relaunch-policy. An empty mode, or the field's default, clears the
// stored row.
func serveRelaunchPolicy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, relaunchPolicyResponse(r))
	case http.MethodPut:
		if relaunchPolicyFromPeer(r) {
			writeError(w, http.StatusForbidden, "peer_read_only", "the relaunch policy is this node's own; another node's operator can read it but not change it")
			return
		}
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
		writeJSON(w, http.StatusOK, relaunchPolicyResponse(r))
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or PUT")
	}
}
