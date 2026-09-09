package model

// SandboxPosture is the provider-classified launch topology. A native mode
// alone cannot distinguish an unconfined process from a host-wrapped process.
// The relation preserves the v1 reviewed cross-provider delegation matrix; it
// does not claim that different providers implement identical filesystems.
type SandboxPosture string

const (
	SandboxPostureUnknown         SandboxPosture = ""
	SandboxPostureUnconfined      SandboxPosture = "unconfined"
	SandboxPostureClaudeInherited SandboxPosture = "claude_inherited"
	SandboxPostureClaudeConfined  SandboxPosture = "claude_confined"
	SandboxPostureCodexReadOnly   SandboxPosture = "codex_read_only"
	SandboxPostureCodexWorkspace  SandboxPosture = "codex_workspace"
	SandboxPostureCodexManaged    SandboxPosture = "codex_managed"
	SandboxPostureCopilotHost     SandboxPosture = "copilot_host"
	SandboxPostureOpenCodeHost    SandboxPosture = "opencode_host"
	SandboxPostureOpenCodeAccess  SandboxPosture = "opencode_access"
)

func (p SandboxPosture) Known() bool {
	switch p {
	case SandboxPostureUnconfined, SandboxPostureClaudeInherited, SandboxPostureClaudeConfined, SandboxPostureCodexReadOnly, SandboxPostureCodexWorkspace, SandboxPostureCodexManaged, SandboxPostureCopilotHost, SandboxPostureOpenCodeHost, SandboxPostureOpenCodeAccess:
		return true
	default:
		return false
	}
}

func (p SandboxPosture) Allows(child SandboxPosture) bool {
	if !p.Known() || !child.Known() {
		return false
	}
	if p == SandboxPostureUnconfined {
		return true
	}
	switch p {
	case SandboxPostureCodexReadOnly:
		return child == SandboxPostureCodexReadOnly
	case SandboxPostureCodexWorkspace:
		return child == SandboxPostureCodexReadOnly || child == SandboxPostureCodexWorkspace
	case SandboxPostureClaudeInherited, SandboxPostureCodexManaged:
		if child == SandboxPostureClaudeInherited {
			return true
		}
	case SandboxPostureOpenCodeAccess:
		if child == SandboxPostureOpenCodeAccess || child == SandboxPostureOpenCodeHost {
			return true
		}
	case SandboxPostureOpenCodeHost:
		if child == SandboxPostureOpenCodeHost {
			return true
		}
	}
	return child == SandboxPostureClaudeConfined || child == SandboxPostureCodexReadOnly || child == SandboxPostureCodexWorkspace || child == SandboxPostureCodexManaged || child == SandboxPostureCopilotHost
}
