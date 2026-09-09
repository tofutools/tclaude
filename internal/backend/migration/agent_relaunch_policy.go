package migration

import (
	"fmt"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// applyAgentRelaunchPolicy restores the normal resolved posture rather than
// reconstructing it from a birth request that may omit inherited choices.
// Source evidence remains verbatim; no current profile defaults are consulted.
func applyAgentRelaunchPolicy(values map[string]any, desired model.DesiredConfiguration) (model.DesiredConfiguration, error) {
	profile, err := decodeAgentRelaunchConfiguration(values)
	if err != nil || profile == nil {
		return desired, err
	}
	if profile.Tools != nil && desired.Harness == "opencode" {
		desired.ToolGovernance = *profile.Tools
	}
	if profile.Model != nil {
		desired.Model = *profile.Model
	}
	if profile.Effort != nil {
		desired.Effort = *profile.Effort
	}
	// Reuse the same provider-specific native vocabulary as profile and explicit
	// birth-request conversion. Do not reinterpret one provider's mode as another.
	native := map[string]any{"harness": desired.Harness}
	if profile.Approval != nil {
		native["approval"] = *profile.Approval
		translated := desiredFromRow(native)
		if translated.Approval == "" && *profile.Approval != "" {
			return desired, fmt.Errorf("unsupported %s resolved approval %q", desired.Harness, *profile.Approval)
		}
		desired.Approval = translated.Approval
	}

	return desired, nil
}
