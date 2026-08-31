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
// pkg/claude/agentd/gitproxy.go and docs/proxies.md.
package proxy

import (
	"encoding/json"
	"io"
	"net/http"
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
//
// The local read never distinguishes the two cases on its own: a sandbox that
// denies ~/.tclaude/data by mounting it empty makes an enabled config look
// exactly like an absent one, error-free. Only ManagedAgentProcess separates
// "the operator has no policy" from "I am not allowed to see it".
func Configured() bool {
	// Any configured family registers the command tree. LinearProxyConfigured
	// rather than LinearProxyEnabled, for the reason the daemon's own
	// visibility check uses it: a key with no allow-list is the scope-only
	// posture, and the command has to exist for a scoped grant to be usable.
	// A key supplied only through LINEAR_API_KEY is invisible from here — that
	// is the DAEMON's environment, and a managed agent reaches the same answer
	// through the /v1/info projection below.
	cfg, err := config.Load()
	if err == nil &&
		(cfg.GitProxyEnabled() || cfg.LinearProxyConfigured() || cfg.AWBProxyEnabled()) {
		return true
	}
	// Do not turn every ordinary host-side command construction into a daemon
	// round trip. Only managed agents need the projection because only their
	// private config view is deliberately reduced to defaults.
	if !agentipc.ManagedAgentProcess() {
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
		Long: "Perform Git-remote, GitHub, Linear and AWB operations WITHOUT holding the credentials yourself.\n\n" +
			"`tclaude agentd` runs git and gh on the host, where the SSH key and GitHub token live, and " +
			"calls Linear's and AWB's APIs with the operator's credentials. Your sandbox can deny ~/.ssh " +
			"and ~/.config/gh outright, and hold no tracker credentials at all, and you can still fetch, " +
			"push, open a pull request, and update your ticket.\n\n" +
			"You describe the operation (\"push my branch\"); the daemon builds the command. There is no " +
			"passthrough flag, no way to influence the argv it runs, and no raw-query escape hatch.\n\n" +
			"Every verb needs a permission slug the operator grants: `proxy.git.read`, `proxy.git.push`, " +
			"`proxy.github.read`, `proxy.github.write`, `proxy.github.merge`, `proxy.linear.read`, " +
			"`proxy.linear.write`, `proxy.awb.read`, `proxy.awb.write`. None is granted by default — " +
			"ask the operator, or pass --ask-human for a one-off approval.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			gitCmd(),
			githubCmd(),
			linearCmd(),
			awbCmd(),
		},
	}.ToCobra()
}
