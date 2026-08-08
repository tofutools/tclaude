package agentd

import (
	"context"
	"fmt"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/portowner"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

const (
	// copilotAPIStartupTimeout bounds the wait for a freshly launched pane's
	// embedded server. Measured behaviour, not a guess: the listener comes up a
	// few seconds after exec, behind auth and workspace initialisation, and
	// TCL-1052's live harness settled on the same 60s ceiling for the same
	// reason. Generous on purpose — the cost of waiting too long is a slow
	// error, and the cost of waiting too little is a working agent declared
	// broken on a loaded host.
	copilotAPIStartupTimeout = 60 * time.Second
	// copilotAPIPollInterval paces the /proc reads. Each poll is two small
	// filesystem walks, so this is cheap enough to keep tight and slow enough
	// not to spin.
	copilotAPIPollInterval = 200 * time.Millisecond
)

// verifiedCopilotAPIPort returns the loopback port of an API-driven Copilot
// agent, and returns it ONLY once the listening socket has been positively
// observed inside the pane process's subtree.
//
// Nothing is sent to the port here, and no caller may send anything to a port
// this function did not return. `--ui-server` has no authentication of any kind
// (TCL-1055), so this proof is the entire access-control story: it is the only
// thing standing between agentd and whatever else won the race for a port
// tclaude reserved, released, and could not hand directly to the harness.
//
// The proof is deliberately made WITHOUT touching the socket. Even opening a
// connection to decide reachability would mean connecting to a process not yet
// known to be ours, so reachability and ownership are both read out of the
// kernel's own tables instead. See portowner.
//
// A mismatch is a hard failure with no fallback. There is no weaker mode to
// degrade to — an unauthenticated endpoint that is not provably the agent's is
// simply not the agent's — so the only safe answers are "verified" and "error".
//
// Retried rather than refused on the first negative. An unowned listener is
// the failure this guards against, but it is not distinguishable, in a single
// sample, from a /proc read that raced the harness's own bind. Retrying costs a
// lost race the full timeout and costs a healthy launch nothing; refusing on
// one sample would turn an ordinary startup race into a spurious hard failure.
// The verdict is still hard — it is just taken at the deadline rather than at
// the first glance — and the error says which of the two it was.
//
// The pane pid the proof was taken AGAINST is returned alongside the port, so a
// caller holding a long-lived connection can re-ask about the same subtree
// rather than resolving one again later. Re-resolving would answer a subtly
// different question — "which pane runs this conversation now" — and after a
// relaunch that is a different pane, which would let a re-proof pass while the
// connection was still attached to the predecessor. See
// copilotAPISession.StillOwned.
func verifiedCopilotAPIPort(ctx context.Context, convID string) (int, int, error) {
	if convID == "" {
		return 0, 0, fmt.Errorf("verify Copilot API port: no conversation id")
	}
	runtime, err := db.GetCopilotAPIRuntime(convID)
	if err != nil {
		return 0, 0, fmt.Errorf("read Copilot API port record for %s: %w", convID, err)
	}
	if runtime == nil {
		return 0, 0, fmt.Errorf(
			"conversation %s has no recorded Copilot API port: it was not launched "+
				"with the API drive, or its pane has already exited and released the record",
			convID)
	}
	port := runtime.Port

	deadline := time.Now().Add(copilotAPIStartupTimeout)
	sawPane := false
	sawListener := false
	for {
		panePID := copilotAPIPanePID(convID)
		if panePID > 0 {
			sawPane = true
			if portowner.ProcessOwnsLoopbackPort(panePID, port) {
				return port, panePID, nil
			}
		}
		if portowner.HasLoopbackListener(port) {
			sawListener = true
		}
		if time.Now().After(deadline) {
			return 0, 0, copilotAPIUnverifiedError(convID, port, sawPane, sawListener)
		}
		select {
		case <-ctx.Done():
			return 0, 0, fmt.Errorf(
				"gave up verifying Copilot API port %d for %s: %w", port, convID, ctx.Err())
		case <-time.After(copilotAPIPollInterval):
		}
	}
}

// copilotAPIConversationsWithARecordedPort lists the conversations that once
// had an endpoint, WITHOUT handing back any of their ports.
//
// It lives here, beside the verified accessor, because it is the only way to
// discover that a conversation has an endpoint at all — and the rule this file
// exists to hold is that everything about the endpoint goes through the proof.
// A caller that enumerated the records somewhere else would be one field away
// from dialling one, and the AST guard names this function's underlying db call
// for that reason.
//
// Conv ids only, so a caller learns which conversations to ASK about and
// nothing it could act on directly. The answer is a list of conversations that
// were given a number, not a list of live endpoints: a row here survives the
// pane that owned it until the reaper releases it.
func copilotAPIConversationsWithARecordedPort() ([]string, error) {
	convIDs, err := db.ListCopilotAPIRuntimeConvIDs()
	if err != nil {
		return nil, fmt.Errorf("list conversations with a recorded Copilot API port: %w", err)
	}
	return convIDs, nil
}

// copilotAPIUnverifiedError names WHICH of the failures happened, because the
// three have completely different remedies and a single "port not reachable"
// would send an operator looking in the wrong place.
//
// Every branch is computed from what the wait actually observed, never from
// what it expected to observe.
func copilotAPIUnverifiedError(convID string, port int, sawPane, sawListener bool) error {
	switch {
	case !sawPane:
		return fmt.Errorf(
			"copilot API port %d for %s could not be verified: no live pane process was "+
				"found for the conversation within %s, so the agent never started or has "+
				"already exited", port, convID, copilotAPIStartupTimeout)
	case !sawListener:
		return fmt.Errorf(
			"copilot API port %d for %s could not be verified: the pane is running but "+
				"nothing ever listened on the port within %s — check the pane for a Copilot "+
				"startup prompt (folder trust blocks the TUI even though the port would "+
				"otherwise be up) or a launch error", port, convID, copilotAPIStartupTimeout)
	default:
		return fmt.Errorf(
			"copilot API port %d for %s is held by a process outside the agent's pane "+
				"subtree: tclaude reserved the port and lost it before the pane could bind "+
				"it. Refusing to talk to it — this endpoint has no authentication, so an "+
				"unverified listener cannot be told apart from another agent's or an "+
				"unrelated process's. Relaunch the agent to allocate a new port",
			port, convID)
	}
}

// copilotAPIPanePID resolves the pane process a conversation's harness runs
// under, which is the root of the subtree the listener must belong to.
//
// The pane pid rather than a recorded harness pid, because that is the identity
// tclaude actually anchors an agent's liveness to, and because copilot runs
// some hops below it: under the pane shell, and under the tclaude-layer wrap
// when one is applied. A subtree walk from the pane covers all of them without
// tclaude having to predict the shape.
//
// 0 means no live session, which the caller treats as "not yet" rather than as
// a verdict — a pane mid-spawn and a pane that died look the same here, and the
// bounded wait is what tells them apart.
func copilotAPIPanePID(convID string) int {
	live := session.LiveSessionForConv(convID)
	if live == nil || live.TmuxSession == "" {
		return 0
	}
	return livePanePID(live.TmuxSession)
}
