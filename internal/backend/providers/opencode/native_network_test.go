package opencode

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
	"testing"
)

func TestNativeWebApprovalFollowsAdmittedNetwork(t *testing.T) {
	for _, tc := range []struct {
		approval model.ApprovalMode
		network  model.SandboxNetworkBaseline
		action   string
	}{
		{model.ApprovalAsk, model.SandboxNetworkInherit, "ask"},
		{model.ApprovalAsk, model.SandboxNetworkAllow, "ask"},
		{model.ApprovalAsk, model.SandboxNetworkDeny, "deny"},
		{model.ApprovalAllowTools, model.SandboxNetworkInherit, "ask"},
		{model.ApprovalAllowTools, model.SandboxNetworkAllow, "allow"},
		{model.ApprovalAllowTools, model.SandboxNetworkDeny, "deny"},
	} {
		rules := permissionRules(tc.approval, model.SandboxUnconfined, tc.network)
		for _, permission := range []string{"webfetch", "websearch"} {
			require.Contains(t, rules, permissionRule{Permission: permission, Pattern: "*", Action: tc.action})
		}
		require.Contains(t, rules, permissionRule{Permission: "bash", Pattern: "*", Action: "allow"})
		require.Contains(t, rules, permissionRule{Permission: "read", Pattern: "*.env", Action: "ask"})
	}
}

func TestNativeNetworkCompositionAndEvidenceRetainDeniedScope(t *testing.T) {
	require.Equal(t, model.SandboxNetworkInherit, nativeNetworkBaseline(nil))
	policy := &sandboxpolicy.PolicyMaterialization{}
	policy.Composition.NetworkAll = []sandboxpolicy.NetworkConjunct{{Policy: model.SandboxNetwork{Baseline: model.SandboxNetworkAllow}}}
	require.Equal(t, model.SandboxNetworkAllow, nativeNetworkBaseline(policy))
	policy.Composition.NetworkAll = append(policy.Composition.NetworkAll, sandboxpolicy.NetworkConjunct{Policy: model.SandboxNetwork{Baseline: model.SandboxNetworkDeny}})
	baseline := nativeNetworkBaseline(policy)
	require.Equal(t, model.SandboxNetworkDeny, baseline)
	envelope, err := encodeEvidence(evidence{NativeNetwork: baseline})
	require.NoError(t, err)
	// A later profile edit cannot change the already recorded network projection.
	policy.Composition.NetworkAll = nil
	restored, err := decodeEvidence(envelope)
	require.NoError(t, err)
	require.Equal(t, model.SandboxNetworkDeny, restored.NativeNetwork)
}
