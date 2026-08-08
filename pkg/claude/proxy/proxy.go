// Package proxy implements `tclaude proxy` — operations agentd performs on an
// agent's behalf with credentials the agent deliberately does not hold.
//
// It is a sibling of `tclaude agent`, not a part of it, because the two answer
// different questions. `tclaude agent` is about COORDINATION: who else exists,
// what am I, send them a message, spawn one. `tclaude proxy` is about
// DELEGATED CREDENTIALS: the agent describes an operation it cannot perform
// itself, and the daemon performs it on the host where the SSH key and the
// GitHub token live. Anything else that follows that shape belongs here too.
//
// The CLI half is deliberately thin. Every gate — which repository, which
// remote, which ref, which permission slug — lives in the daemon, because a
// check made in this process is a check the caller could have skipped. See
// pkg/claude/agentd/gitproxy.go and docs/git-proxy.md.
package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common"
)

// Exit codes. Aliases rather than fresh literals: agent.MapDaemonErrorToRC
// returns these values, so they have to agree, and a `const x = agent.RCx`
// cannot drift the way a copied number can.
const (
	rcOK         = agent.RCOK
	rcInvalidArg = agent.RCInvalidArg
	rcIOFailure  = agent.RCIOFailure
)

// Configured reports whether the operator has opted into any semantic proxy
// policy. A normal host CLI can answer from config directly. A managed agent
// cannot read the private config tree, so it asks agentd for the same one-bit
// capability projection; none of the allow-list or credential policy crosses
// the sandbox boundary.
func Configured() bool {
	cfg, err := config.Load()
	if err == nil && cfg.GitProxyEnabled() {
		return true
	}
	// Do not turn every ordinary host-side command construction into a daemon
	// round trip. Only managed agents need the projection because only their
	// private config view is deliberately reduced to defaults.
	managedAgent := os.Getenv(agentipc.SocketEnv) != "" ||
		os.Getenv("CODEX_PERMISSION_PROFILE") == "tclaude-agent"
	if !managedAgent {
		return false
	}
	var info struct {
		Proxy *bool `json:"proxy"`
	}
	if err := agent.DaemonRequest(http.MethodGet, "/v1/info", nil, &info,
		agent.DaemonOpts{
			Timeout:     250 * time.Millisecond,
			RetryOutput: io.Discard,
			NoRetry:     true,
		}); err != nil {
		return false
	}
	// Daemons predating the capability projection omit the field. Preserve
	// their historical always-visible command tree until they are restarted
	// on a version that can report the operator's effective policy.
	return info.Proxy == nil || *info.Proxy
}

// writeJSONIndent renders a --json response. Indented, because these bodies are
// read by an agent deciding what to do next as often as by a script.
func writeJSONIndent(w io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return rcIOFailure
	}
	return rcOK
}

// Cmd returns the `tclaude proxy` cobra command.
func Cmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "proxy",
		Short: "Operations the daemon performs for you with credentials you do not hold",
		Long: "Perform Git-remote and GitHub operations WITHOUT holding the credentials yourself.\n\n" +
			"`tclaude agentd` runs git and gh on the host, where the SSH key and GitHub token live. " +
			"Your sandbox can deny ~/.ssh and ~/.config/gh outright and you can still fetch, push, and " +
			"open a pull request.\n\n" +
			"You describe the operation (\"push my branch\"); the daemon builds the command. There is no " +
			"passthrough flag and no way to influence the argv it runs.\n\n" +
			"Every verb needs a permission slug the operator grants: `git.read`, `git.push`, " +
			"`github.read`, `github.write`. None is granted by default — ask the operator, or pass " +
			"--ask-human for a one-off approval.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			gitCmd(),
			githubCmd(),
		},
	}.ToCobra()
}
