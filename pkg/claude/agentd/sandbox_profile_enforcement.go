package agentd

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

type sandboxProfileEnforcementTarget struct {
	Implementation string                      `json:"implementation"`
	Harness        string                      `json:"harness"`
	Platform       string                      `json:"platform"`
	Predicted      bool                        `json:"predicted"`
	Axes           harness.PredictedAccessAxes `json:"axes"`
	Caveat         string                      `json:"caveat,omitempty"`
}

type sandboxProfileEnforcementResponse struct {
	Profile string                            `json:"profile"`
	Targets []sandboxProfileEnforcementTarget `json:"targets"`
}

type parsedSandboxProfileEnforcementTarget struct {
	implementation sandboxpolicy.Implementation
	harness        *harness.Harness
	platform       string
	raw            string
}

func handleSandboxProfileEnforcement(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "missing sandbox profile name")
		return
	}
	rawTargets := r.URL.Query()["for"]
	if len(rawTargets) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "at least one for target is required")
		return
	}
	profile, err := db.GetSandboxProfile(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if profile == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such sandbox profile")
		return
	}
	flattened, err := flattenSandboxProfileForPrediction(profile)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", err.Error())
		return
	}
	axes, err := sandboxpolicy.DeriveAccessAxes(flattened)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", err.Error())
		return
	}
	response := sandboxProfileEnforcementResponse{
		Profile: profile.Name,
		Targets: make([]sandboxProfileEnforcementTarget, 0, len(rawTargets)),
	}
	for _, raw := range rawTargets {
		target, err := parseSandboxProfileEnforcementTarget(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		mode := predictedBuiltinMode(target.harness.Name)
		prediction, err := harness.PredictAccessEnforcement(
			target.harness, target.implementation, axes, mode, target.platform,
		)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg",
				fmt.Sprintf("invalid --for target %q: %v", raw, err))
			return
		}
		item := sandboxProfileEnforcementTarget{
			Implementation: string(target.implementation),
			Harness:        target.harness.Name,
			Platform:       target.platform,
			Predicted:      true,
			Axes:           harness.DescribePredictedAccess(axes, prediction),
		}
		if target.platform != runtime.GOOS {
			item.Caveat = "(prediction for a non-host platform; host capability probes did not run)"
		} else if target.implementation.UsesTclaudeLayer() {
			probe := cachedTclaudeLayerHostAvailability
			if err := probe(); err != nil {
				item.Caveat = "(prediction only; the target's host capability could not be verified: " + err.Error() + ")"
			}
		}
		response.Targets = append(response.Targets, item)
	}
	writeJSON(w, http.StatusOK, response)
}

func flattenSandboxProfileForPrediction(profile *db.SandboxProfile) (sandboxpolicy.Profile, error) {
	registryRows, err := db.ListSandboxProfiles()
	if err != nil {
		return sandboxpolicy.Profile{}, err
	}
	registry := make(map[string]*sandboxpolicy.Profile, len(registryRows))
	for _, row := range registryRows {
		registry[row.Name] = sandboxProfileDBToPolicy(row)
	}
	root := registry[profile.Name]
	if root == nil {
		root = sandboxProfileDBToPolicy(profile)
	}
	return sandboxpolicy.Flatten(*root, func(name string) (*sandboxpolicy.Profile, error) {
		return registry[name], nil
	})
}

func sandboxProfileDBToPolicy(profile *db.SandboxProfile) *sandboxpolicy.Profile {
	if profile == nil {
		return nil
	}
	return &sandboxpolicy.Profile{
		Name: profile.Name, Filesystem: profile.Filesystem,
		Environment: profile.Environment, AgentDirectories: profile.AgentDirectories,
		NetworkAccess: profile.NetworkAccess, Network: profile.Network,
		UnixSockets: profile.UnixSockets, Includes: profile.Includes,
	}
}

func parseSandboxProfileEnforcementTarget(raw string) (parsedSandboxProfileEnforcementTarget, error) {
	raw = strings.TrimSpace(raw)
	parts := strings.Split(raw, "/")
	if raw == "" || len(parts) > 3 {
		return parsedSandboxProfileEnforcementTarget{}, invalidSandboxProfileTarget(raw)
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return parsedSandboxProfileEnforcementTarget{}, invalidSandboxProfileTarget(raw)
		}
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(parts[0])
	if err != nil {
		return parsedSandboxProfileEnforcementTarget{}, invalidSandboxProfileTarget(raw)
	}
	harnessName := harness.DefaultName
	if len(parts) >= 2 {
		harnessName = parts[1]
	}
	switch harnessName {
	case harness.DefaultName, harness.CodexName, harness.OpenCodeName:
	default:
		return parsedSandboxProfileEnforcementTarget{}, invalidSandboxProfileTarget(raw)
	}
	h, err := harness.Resolve(harnessName)
	if err != nil {
		return parsedSandboxProfileEnforcementTarget{}, invalidSandboxProfileTarget(raw)
	}
	platform := runtime.GOOS
	if len(parts) == 3 {
		platform = parts[2]
	}
	if platform != "linux" && platform != "darwin" {
		return parsedSandboxProfileEnforcementTarget{}, invalidSandboxProfileTarget(raw)
	}
	return parsedSandboxProfileEnforcementTarget{
		implementation: implementation, harness: h, platform: platform, raw: raw,
	}, nil
}

func invalidSandboxProfileTarget(raw string) error {
	return fmt.Errorf(
		`invalid --for target %q (want implementation[/harness[/platform]]; `+
			`implementation: harness-builtin, tclaude-layer, stacked; `+
			`harness: claude, codex, opencode; platform: linux, darwin)`,
		raw,
	)
}

func predictedBuiltinMode(harnessName string) string {
	switch harnessName {
	case harness.DefaultName:
		return harness.ClaudeSandboxOn
	case harness.CodexName:
		return harness.SandboxManagedProfile
	case harness.OpenCodeName:
		return harness.OpenCodeSandboxAccessControl
	default:
		return ""
	}
}
