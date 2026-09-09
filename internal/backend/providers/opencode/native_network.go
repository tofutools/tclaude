package opencode

import (
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

// Preserve the admitted projection across recovery; rebuilding it from mutable
// profile settings would change the permission of an already running session.
func nativeNetworkBaseline(policy *sandboxpolicy.PolicyMaterialization) model.SandboxNetworkBaseline {
	baseline := model.SandboxNetworkInherit
	if policy == nil {
		return baseline
	}
	for _, term := range policy.Composition.NetworkAll {
		if term.Policy.Baseline == model.SandboxNetworkDeny {
			return model.SandboxNetworkDeny
		}
		if term.Policy.Baseline == model.SandboxNetworkAllow {
			baseline = model.SandboxNetworkAllow
		}
	}
	return baseline
}
