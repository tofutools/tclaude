// Package lineage shares structural checks on provider-decoded confinement
// evidence. It never reads native/private files or interprets vendor policies.
package lineage

import (
	"encoding/hex"
	"path/filepath"

	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func Matches(execution model.Execution, recordedID, policyHash string, artifact *host.SandboxChildArtifact) bool {
	if !Resolved(execution.Spec) || execution.ID == "" || execution.ID != execution.Spec.ExecutionID || string(execution.ID) != recordedID {
		return false
	}
	if execution.Spec.HostSandbox == nil {
		return policyHash == "" && artifact == nil
	}
	selected := execution.Spec.HostSandbox
	if selected.OmitProfiles || selected.PolicyHash == "" || selected.PolicyHash != policyHash || artifact == nil || !filepath.IsAbs(artifact.Path) {
		return false
	}
	digest, err := hex.DecodeString(artifact.Digest)
	return err == nil && len(digest) == 32
}

// Resolved refuses saved choices which have not yet been materialized for a
// launch. A selected profile ID by itself is not evidence of an outer wall.
func Resolved(spec model.ResolvedExecutionSpec) bool {
	if spec.HostSandbox == nil {
		return true
	}
	digest, err := hex.DecodeString(spec.HostSandbox.PolicyHash)
	return !spec.HostSandbox.OmitProfiles && err == nil && len(digest) == 32
}
