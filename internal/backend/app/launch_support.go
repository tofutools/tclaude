package app

import (
	"context"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
)

type LaunchSupportRequest struct {
	Principal model.Principal
	Harness   string
}
type LaunchSupportResult struct {
	Harness              string
	Configured           bool
	PolicyKnown          bool
	ApprovalModes        []model.ApprovalMode
	SandboxModes         []model.SandboxMode
	PreparedInitialInput bool
	HostSandbox          bool
	Basis                string
}

// LaunchSupport reads the configured adapter's declared contract without native
// preparation, capability probing, credential delivery, or durable effects.
func (s *Service) LaunchSupport(_ context.Context, req LaunchSupportRequest) (LaunchSupportResult, error) {
	if err := requireOperator(req.Principal); err != nil {
		return LaunchSupportResult{}, err
	}
	if strings.TrimSpace(req.Harness) == "" || strings.TrimSpace(req.Harness) != req.Harness || len(req.Harness) > 128 {
		return LaunchSupportResult{}, ErrInvalid
	}
	result := LaunchSupportResult{Harness: req.Harness, Basis: "Configured adapter policy support only; installation, authentication, current authority and runtime readiness are checked separately at launch."}
	if s.providers == nil {
		return result, nil
	}
	provider, ok := s.providers.Provider(req.Harness)
	if !ok {
		return result, nil
	}
	result.Configured = true
	capabilities := provider.Capabilities()
	result.PreparedInitialInput = capabilities.PreparedInitialInput
	result.HostSandbox = capabilities.HostSandbox
	if capabilities.LaunchPolicy != nil {
		result.PolicyKnown = true
		result.ApprovalModes = append([]model.ApprovalMode(nil), capabilities.LaunchPolicy.SupportedApproval...)
		result.SandboxModes = append([]model.SandboxMode(nil), capabilities.LaunchPolicy.SupportedSandbox...)
	}
	return result, nil
}
