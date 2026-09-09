package codex

import (
	"context"
	"os"
	"os/exec"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

// Legacy approval values remain durable, but newer CLIs removed them. Probe
// argument parsing only: --help cannot start a conversation or require login.
func (p *Provider) launchPolicy() ports.PolicyRequirements {
	p.policyOnce.Do(func() {
		p.policy = supportedLaunchPolicy()
		if p.executable == "" {
			return
		}
		for _, mode := range []model.ApprovalMode{model.ApprovalOnFailure, model.ApprovalUntrusted} {
			if acceptsApproval(p.executable, mode) {
				p.policy.SupportedApproval = append(p.policy.SupportedApproval, mode)
			}
		}
	})
	return p.policy
}

func acceptsApproval(executable string, mode model.ApprovalMode) bool {
	root, err := os.MkdirTemp("", "codex-approval-capability-")
	if err != nil {
		return false
	}
	defer func() { _ = os.RemoveAll(root) }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-a", string(mode), "--help")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HOME="+root, "CODEX_HOME="+root)
	cmd.WaitDelay = 100 * time.Millisecond
	return cmd.Run() == nil
}
