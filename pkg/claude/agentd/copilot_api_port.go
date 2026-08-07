package agentd

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// copilotAPILoopbackFailure refuses an API-driven Copilot launch whose sandbox
// would put the pane in its own network namespace.
//
// The API drive is a TCP channel on 127.0.0.1 that agentd opens to a process it
// does not itself launch: it allocates the port, forks `tclaude session new`,
// and that builds the copilot argv. Under a network posture that unshares the
// namespace, the port copilot binds is a DIFFERENT 127.0.0.1 from the one
// agentd holds, and no amount of waiting makes it appear. See TCL-1054.
//
// Refusing here rather than letting the launch proceed is the same call tclaude
// already makes for every other unsupported sandbox combination: the operator
// gets a named reason back from the spawn API instead of watching an agent come
// up and then fail on an unreachable port, in a different place, looking like a
// Copilot bug. It is also the honest boundary — nothing downstream can repair
// this, so a later failure would only be a slower way to say the same thing.
//
// The posture named in the message is the one TclaudeLayerSharesHostLoopback
// decided on, not a second reading of the profile. A refusal that names a
// posture the launch would not have used is worse than a generic refusal,
// because it sends whoever is debugging it at the wrong setting.
//
// Silent on a non-Copilot harness and on a launch that did not ask for the API
// drive: this gates the channel, not the sandbox.
func copilotAPILoopbackFailure(
	copilotAPI bool,
	snapshot *sandboxpolicy.Snapshot,
	sandboxImplementation string,
) *spawnFailure {
	if !copilotAPI {
		return nil
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(sandboxImplementation)
	if err != nil {
		return &spawnFailure{
			http.StatusUnprocessableEntity, "invalid_sandbox_implementation", err.Error()}
	}
	effective := sandboxpolicy.EffectiveProfile{}
	if snapshot != nil {
		effective = snapshot.Effective
	}
	shares, posture, err := session.TclaudeLayerSharesHostLoopback(implementation, effective)
	if err != nil {
		return &spawnFailure{
			http.StatusUnprocessableEntity, "invalid_sandbox_profile", err.Error()}
	}
	if shares {
		return nil
	}
	return &spawnFailure{
		http.StatusUnprocessableEntity,
		"copilot_api_unreachable_network_posture",
		fmt.Sprintf(
			"the API-backed Copilot drive needs the pane to share host loopback, but this "+
				"launch resolves to the %q network posture, which gives the pane a private "+
				"network namespace that agentd cannot reach — spawn without the API drive, "+
				"or use a sandbox profile whose network access is open (%q)",
			posture, sandboxpolicy.NetworkHostOpen.String()),
	}
}

// prepareCopilotAPIPort allocates this launch's Copilot API port and writes it
// into args, returning the conversation the record will be keyed by.
//
// Called from the two spawn facades, which is the last point where BOTH the
// number and the launch are in one place: below here the argv has been rendered
// and forked, and above here there are four SpawnArgs construction sites that
// would each have to remember to do this.
//
// A launch that asked for the API drive but has no conversation id is refused
// rather than launched without a record. The pane would come up and listen, and
// agentd would have no way to find the port again — an agent that looks healthy
// and is unreachable, which is the failure this whole ticket exists to avoid.
//
// Nothing is written to the database here. The record is the caller's job and
// belongs strictly after a successful hand-off; a port recorded before the
// spawn outlives a spawn that failed.
func prepareCopilotAPIPort(args *clcommon.SpawnArgs, convID string) (string, error) {
	if !args.CopilotAPI {
		return "", nil
	}
	if convID == "" {
		return "", fmt.Errorf(
			"the API-backed Copilot drive needs the conversation id before launch, " +
				"so the allocated port can be recorded against it")
	}
	port, err := allocateCopilotAPIPort()
	if err != nil {
		return "", err
	}
	args.CopilotAPIPort = port
	return convID, nil
}

// recordCopilotAPIPort persists the port a launch was handed, AFTER the launch
// has been handed off.
//
// Best-effort by design. The pane is already starting by the time this runs, so
// returning an error here would report a failed spawn for a launch that
// succeeded — a worse lie than the missing row, and one that would send a
// caller into rollback. The missing row is loud on its own: the next lookup
// finds nothing and says so.
func recordCopilotAPIPort(convID string, port int) {
	if convID == "" || port <= 0 {
		return
	}
	if err := db.UpsertCopilotAPIRuntime(db.CopilotAPIRuntime{
		ConvID: convID, Port: port,
	}); err != nil {
		slog.Error("failed to record Copilot API port",
			"conv_id", convID, "port", port, "error", err)
	}
}

// releaseCopilotAPIPortForExit drops the port claim of a Copilot conversation
// whose pane is gone.
//
// Harness-gated so the reaper's hot loop does not query the database once per
// non-Copilot agent per tick. A send-keys Copilot agent has no row either, and
// deleting nothing is cheap, so the gate is on the harness rather than on the
// drive — the drive is not on the state this loop reads, and fetching it would
// cost more than the delete it would save.
func releaseCopilotAPIPortForExit(harnessName, convID string) {
	if harnessName != harness.CopilotName {
		return
	}
	releaseCopilotAPIPort(convID)
}

// releaseCopilotAPIPort drops a conversation's port claim.
//
// The OS reclaimed the port when the process died; what is dropped here is
// tclaude's record that the port MEANT something. Keeping it would leave a
// number that still looks answerable and, after reuse, may be answered by
// something else entirely.
func releaseCopilotAPIPort(convID string) {
	if convID == "" {
		return
	}
	if err := db.DeleteCopilotAPIRuntime(convID); err != nil {
		slog.Warn("failed to release Copilot API port record",
			"conv_id", convID, "error", err)
	}
}

// allocateCopilotAPIPort picks a free loopback port by binding it and letting
// it go again.
//
// This leaves an unavoidable bind-close-exec gap, exactly as the OpenCode
// server launch does and for the same reason: copilot cannot be handed a
// pre-bound listener, so tclaude can only choose a number and hope to win the
// race to it. The gap is why ownership verification exists — nothing may be
// sent to this port until the launched pid's subtree is positively observed
// owning the listening socket, because `--ui-server` has no authentication of
// any kind to fall back on (TCL-1055).
//
// Binding explicitly on 127.0.0.1 rather than on all interfaces keeps the
// reservation, the harness's own bind, and the ownership proof talking about
// one address.
func allocateCopilotAPIPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate Copilot API port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, fmt.Errorf("release allocated Copilot API port %d: %w", port, err)
	}
	return port, nil
}
