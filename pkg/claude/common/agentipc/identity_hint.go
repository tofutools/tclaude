package agentipc

import (
	"os"
	"strings"
)

const (
	// AgentHintEnvVar marks a process as probably belonging to a managed agent
	// session. It is only a UX hint: daemon authorization must continue to use
	// peer credentials and harness ancestry, never this caller-controlled value.
	AgentHintEnvVar = "TCLAUDE_AGENT_HINT"

	// AgentHintHeader carries AgentHintEnvVar across the CLI/daemon boundary so
	// identity failures can return agent-oriented recovery guidance.
	AgentHintHeader = "X-Tclaude-Agent-Hint"

	// SessionIDEnvVar is the stable session-row key exported into managed panes.
	// It is caller-controlled and therefore only selects a row for server-side
	// proof; it is never authorization by itself.
	SessionIDEnvVar = "TCLAUDE_SESSION_ID"

	// SessionClaimHeader carries SessionIDEnvVar on agent CLI requests. Agentd
	// accepts it only when Unix peer credentials, the live tmux pane, and the
	// daemon-owned launch generation independently prove the claimed row.
	SessionClaimHeader = "X-Tclaude-Session-ID"

	// CodexPermissionProfileEnv names the Codex CLI's active permission profile,
	// and ManagedCodexProfileName is the value a tclaude-managed Codex session
	// runs under. Together they identify managed Codex sessions that predate
	// SocketEnv and AgentHintEnvVar being exported into agent panes.
	CodexPermissionProfileEnv = "CODEX_PERMISSION_PROFILE"
	ManagedCodexProfileName   = "tclaude-agent"
)

// HasAgentHint reports whether AgentHintEnvVar marks this process as belonging
// to a managed agent session.
func HasAgentHint() bool {
	return strings.TrimSpace(os.Getenv(AgentHintEnvVar)) == "1"
}

// ManagedAgentProcess reports whether this process runs inside a tclaude-managed
// agent session — one whose sandbox deliberately denies ~/.tclaude/data, so
// anything the operator's private config would answer has to be asked of agentd
// instead.
//
// It ORs three signals because no single one covers every managed session:
// sandboxed launches that allowlist the daemon socket pin SocketEnv; managed
// Codex sessions predating that carry CodexPermissionProfileEnv; and every
// agentd-managed launch carries AgentHintEnvVar — which is the only one of the
// three a managed Claude Code pane has.
//
// All three are caller-controlled, so this is a UX signal only. It decides
// which side of the sandbox boundary to ASK, never what the answer may be:
// daemon authorization stays on Unix peer credentials and process ancestry.
func ManagedAgentProcess() bool {
	return os.Getenv(SocketEnv) != "" ||
		os.Getenv(CodexPermissionProfileEnv) == ManagedCodexProfileName ||
		HasAgentHint()
}
