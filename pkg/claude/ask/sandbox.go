package ask

import (
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func resolveAskSandbox(setting, cwd string) (*sandboxpolicy.Snapshot, error) {
	if setting == "" || setting == "off" {
		return nil, nil
	}
	var snapshot sandboxpolicy.Snapshot
	var err error
	switch setting {
	case "global":
		snapshot, err = db.ResolveEffectiveSandboxSnapshot(0, "")
	case "auto":
		group, matchErr := agent.MatchingDirectoryGroup(cwd)
		if matchErr != nil {
			return nil, fmt.Errorf("ask sandbox auto: %w; choose ask.sandbox=global or a named profile to avoid directory matching", matchErr)
		}
		id := int64(0)
		if group != nil {
			id = group.ID
		}
		snapshot, err = db.ResolveEffectiveSandboxSnapshot(id, "")
	default:
		name, _ := config.AskSandboxProfileName(setting)
		snapshot, err = db.ResolveNamedSandboxSnapshot(name)
		if err != nil {
			return nil, fmt.Errorf("ask.sandbox profile %q is unavailable: %w; choose an existing library profile, auto, global, or off", name, err)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("resolve ask sandbox %q: %w", setting, err)
	}
	if len(snapshot.Applied) == 0 {
		return nil, nil
	}
	return &snapshot, nil
}

func prepareAskSandbox(in askInput, h *harness.Harness, spec harness.AskSpec) (*agentd.PreparedOneShotLaunch, string, error) {
	snapshot, err := resolveAskSandbox(in.SandboxSetting, in.Cwd)
	if err != nil {
		return nil, "", err
	}
	if snapshot == nil {
		return nil, "", nil
	}
	implementation, mode, approval := "", "", ""
	for _, profile := range in.LaunchProfiles {
		if profile == nil {
			continue
		}
		source := profile.Harness
		if source == "" {
			source = harness.DefaultName
		}
		if source != h.Name {
			continue
		}
		if profile.ModelProxy != "" && profile.ModelProxy != "off" {
			return nil, "", fmt.Errorf("ask sandbox: model gateway %q needs a tracked gateway caller identity; use an interactive worker or a profile without a model gateway", profile.ModelProxy)
		}
		if implementation == "" {
			implementation = profile.SandboxImplementation
		}
		if mode == "" {
			mode = profile.Sandbox
		}
		if approval == "" {
			approval = profile.Approval
		}
	}
	if implementation == "" {
		implementation = string(sandboxpolicy.ImplementationTclaudeLayer)
	}
	impl, err := sandboxpolicy.NormalizeImplementation(implementation)
	if err != nil {
		return nil, "", fmt.Errorf("ask sandbox: %w", err)
	}
	if impl.OmitsOSConfinement() {
		return nil, "", fmt.Errorf("ask sandbox %q cannot be enforced by %s; select ask.sandbox=off or an ask profile with a confined sandbox implementation", in.SandboxSetting, implementation)
	}
	mode, err = harness.ResolveNativeHarnessBuiltinMode(h, mode, impl)
	if err != nil {
		return nil, "", fmt.Errorf("ask sandbox: %w", err)
	}
	if approval != "" {
		if h.Approval == nil {
			return nil, "", fmt.Errorf("ask sandbox: harness %q has no approval-policy contract", h.Name)
		}
		approval, err = h.Approval.ValidatePolicy(approval)
		if err != nil {
			return nil, "", fmt.Errorf("ask sandbox: %w", err)
		}
	}
	prepared, err := agentd.PrepareAskLaunch(agentd.AskLaunchInput{Cwd: in.Cwd, Harness: h, Ask: spec, Snapshot: snapshot, Implementation: implementation, HarnessBuiltinMode: mode, ApprovalPolicy: approval})
	if err != nil {
		return nil, "", err
	}
	var names []string
	for _, profile := range snapshot.Applied {
		names = append(names, fmt.Sprintf("%s %q", profile.Scope, profile.Name))
	}
	disclosure := fmt.Sprintf("ask: sandbox %s (%s; %s)", in.SandboxSetting, strings.Join(names, ", "), implementation)
	return prepared, disclosure, nil
}
