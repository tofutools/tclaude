package agentd

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

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
				"or use a sandbox profile whose network access is open in the shared host "+
				"namespace (%q; network.namespace host or omitted)",
			posture, sandboxpolicy.NetworkHostOpen.String()),
	}
}

// prepareCopilotAPIPort allocates this launch's Copilot API port and writes it
// into args.
//
// Called from the two spawn facades, which is the last point where BOTH the
// number and the launch are in one place: below here the argv has been rendered
// and forked, and above there are several SpawnArgs construction sites that
// would each have to remember to do this.
//
// Allocation is NOT conditional on knowing the conversation id, and the two
// must not be coupled. The port has to be in the argv, so it has to be chosen
// before the fork; the conv id does not, because most launches preset it but
// some MINT it — an unenrolled clone or reincarnation lets the harness choose
// the id and discovers it afterwards from the session row. Requiring the id
// here would refuse those launches outright, which is a strictly worse outcome
// than recording their port a moment later. See recordCopilotAPIPort.
//
// Nothing is written to the database here. The record is the caller's job and
// belongs strictly after a successful hand-off; a port recorded before the
// spawn outlives a spawn that failed.
func prepareCopilotAPIPort(args *clcommon.SpawnArgs) error {
	if !args.CopilotAPI {
		return nil
	}
	port, err := allocateCopilotAPIPort()
	if err != nil {
		return err
	}
	args.CopilotAPIPort = port
	return nil
}

// recordCopilotAPIPort persists the port a launch was handed, AFTER the launch
// has been handed off and once its conversation id is known.
//
// Two call moments, because launches learn their conv id at two different
// times. A launch that PRESET the id (an enrolled spawn, a resume, a copy
// clone) records straight after the fork, from the spawn facade. A launch that
// let the harness MINT the id records at the point it discovers it — the poll
// over the session row that clone and reincarnate already run to find out what
// they launched. Both land well before anything can use the port, which is the
// only ordering that matters.
//
// Called ONLY from completeCopilotAPILaunch, which is what makes those two
// moments two call sites rather than four, and what makes it impossible to
// record a port for a conversation without also recording the drive it belongs
// to. That is enforced rather than asked for — see
// TestCopilotLaunchesRecordPortAndPostureTogether.
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
// that no longer has a launch.
//
// Driven by an EXITED session row, but deliberately not decided by one. Session
// rows are per-launch and exited ones are retained; the port record is per
// CONVERSATION and a relaunch reuses the conv id. So a relaunched agent has a
// live row and its own predecessor's exited row under one id, and releasing on
// the exited row alone would delete the successor's live record — on the next
// tick, and on every tick after, because the predecessor is never cleaned up.
// convsWithLiveLaunch is what makes the exited row's evidence conditional.
//
// The grace covers the other end of the same window. A launch records its port
// immediately after the fork, while the new session row appears a moment later,
// so a tick landing in between sees a conversation with no live launch and a
// record that is nonetheless perfectly good. Refusing to release a record
// younger than the reaper's own spawn grace closes that gap, using the same
// window the reaper already trusts for "this row may just be mid-spawn".
//
// Harness-gated first so the reaper's hot loop does not touch the database once
// per non-Copilot agent per tick.
func releaseCopilotAPIPortForExit(
	harnessName, convID string,
	convsWithLiveLaunch map[string]bool,
	grace time.Duration,
) {
	if harnessName != harness.CopilotName || convID == "" {
		return
	}
	if convsWithLiveLaunch[convID] {
		return
	}
	runtime, err := db.GetCopilotAPIRuntime(convID)
	if err != nil {
		slog.Warn("failed to read Copilot API port record before release",
			"conv_id", convID, "error", err)
		return
	}
	if runtime == nil {
		return
	}
	if time.Since(runtime.UpdatedAt) < grace {
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
	// The live handle goes with the claim. This is housekeeping rather than a
	// correctness mechanism: a handle to a pane that has exited is already dead
	// on its own — its connection closed when the process did, and every read
	// of it reports that — so nothing depends on this running. What it buys is
	// that the registry does not accumulate one dead entry per retired
	// conversation for the lifetime of the daemon.
	copilotAPISessions.Drop(convID)
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
