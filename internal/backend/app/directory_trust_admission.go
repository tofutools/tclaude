package app

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type directoryTrustAdmission struct {
	enabled bool
	path    string
	proven  []string
}

// Caller authority must be checked before this helper issues a challenge. The
// caller's admitted execution supplies its root; tracked editing paths never do.
func (s *Service) prepareDirectoryTrust(ctx context.Context, request RequestContext, desired model.DesiredConfiguration, intent any) (directoryTrustAdmission, error) {
	result, err := s.resolveDirectoryTrust(ctx, request, desired)
	if err != nil || len(result.proven) == 0 {
		return result, err
	}
	proven, err := s.directoryChallenges.verify(ctx, s.directoryProof, request.Principal, struct {
		Intent  any
		Desired model.DesiredConfiguration
	}{intent, desired}, request.WriteProofToken, result.proven, s.now().UTC())
	if err != nil {
		return directoryTrustAdmission{}, err
	}
	result.proven = proven
	return result, nil
}

func (s *Service) resolveDirectoryTrust(ctx context.Context, request RequestContext, desired model.DesiredConfiguration) (directoryTrustAdmission, error) {
	result := directoryTrustAdmission{enabled: desired.TrustDirectory, path: desired.WorkingDirectory}
	if model.ValidateDirectoryTrust(true, desired.Harness) != nil {
		return result, nil
	}
	var sibling []string
	if worktrees, ok := s.directoryProof.(ports.DirectoryTrustWorktree); ok {
		var err error
		sibling, err = worktrees.DefaultSiblingTrustDirectories(ctx, desired.WorkingDirectory)
		if err != nil {
			return result, fail(ErrUnavailable, "inspect launch directory trust: %v", err)
		}
		result.enabled = result.enabled || len(sibling) != 0
	}
	if !result.enabled || OperatorProfileCaller(request.Principal) {
		return result, nil
	}
	if s.directoryProof == nil {
		return result, fail(ErrUnavailable, "directory write-proof support is unavailable")
	}
	parent, err := s.directoryTrustParent(ctx, request.Principal)
	if err != nil {
		return result, err
	}
	paths, err := s.directoryProof.ResolveProofDirectories(ctx, []string{desired.WorkingDirectory})
	if err != nil || len(paths) != 1 {
		return result, fail(ErrInvalid, "launch directory cannot be resolved")
	}
	root, rootErr := s.directoryProof.ResolveProofDirectories(ctx, []string{parent.Spec.WorkingDirectory})
	owned := rootErr == nil && len(root) == 1 && directoryContains(root[0], paths[0])
	if !owned && len(sibling) == 0 {
		return result, fail(ErrUnauthorized, "agents may pre-trust only their own launch directory or a subdirectory, or the default repository sibling worktree; leave directory trust off or ask the operator to launch")
	}
	// V1 exempts only an actually unconfined parent, never a harness-native
	// unconfined spelling inside a tclaude host sandbox.
	if parent.Spec.HostSandbox == nil && parent.Spec.Sandbox == model.SandboxUnconfined && (parent.Spec.Harness == "claude" || parent.Spec.Harness == "codex" || parent.Spec.Harness == "opencode") {
		return result, nil
	}
	if len(sibling) != 0 {
		paths = append(paths, sibling...)
	}
	result.path = paths[0]
	result.proven = paths
	return result, nil
}

func (s *Service) directoryTrustParent(ctx context.Context, principal model.Principal) (model.Execution, error) {
	id, agentID := principal.ExecutionID, principal.AgentID
	if agentID == "" {
		agentID = principal.Authority.AgentID
	}
	if id == "" && agentID != "" {
		agent, err := s.store.Agent(ctx, agentID)
		if err != nil {
			return model.Execution{}, err
		}
		id = agent.PrimaryExecutionID
	}
	if id == "" {
		return model.Execution{}, fail(ErrUnauthorized, "caller has no admitted launch directory")
	}
	execution, err := s.store.Execution(ctx, id)
	if err != nil {
		return execution, err
	}
	if agentID != "" && execution.AgentID != agentID {
		return model.Execution{}, ErrUnauthorized
	}
	return execution, nil
}

func directoryContains(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (s *Service) reassertDirectoryTrust(ctx context.Context, admission directoryTrustAdmission) error {
	if len(admission.proven) == 0 {
		return nil
	}
	if s.directoryProof == nil {
		return fail(ErrUnavailable, "directory write-proof support is unavailable")
	}
	if err := s.directoryProof.ReassertProofDirectories(ctx, admission.proven); err != nil {
		return fail(ErrUnauthorized, "launch directory changed after caller write proof: %v", err)
	}
	return nil
}

func (s *Service) cleanupDirectoryTrust(ctx context.Context, admission directoryTrustAdmission, token string) {
	if len(admission.proven) != 0 {
		_ = s.directoryProof.RemoveProofMarkers(context.WithoutCancel(ctx), admission.proven, token)
	}
}
