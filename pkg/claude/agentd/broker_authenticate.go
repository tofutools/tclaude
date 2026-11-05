package agentd

import (
	"io"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// brokerKind is what one brokered endpoint brings to the identity sequence
// both share (brokerAuthenticate): its path, the noun its messages use for
// its payload, and how its body is read.
type brokerKind struct {
	endpoint string
	// noun names the payload in replies and refusal reasons: "hook" or
	// "statusline".
	noun string
	// omittedClaimMessage answers a layer caller that sent no session claim.
	omittedClaimMessage string
	// rateMessage answers a placed caller over its per-row ceiling.
	rateMessage string
	// decode reads the capped body into the endpoint's own request and
	// returns the session the caller claims and, for a hook, its event.
	decode func(body []byte) (claimed, event string, err error)
}

// brokerCaller is a caller brokerAuthenticate placed: the session row the
// daemon proved owns it, the harness above it, and the request's timing,
// which the handler finishes once it has applied the payload.
type brokerCaller struct {
	row        *db.SessionRow
	harnessPID int
	timing     *brokerTiming
}

// brokerAuthenticate runs the identity sequence of a brokered endpoint up to
// and including the per-row rate charge, and answers the caller itself if
// any step refuses it. A nil result means the request has been answered.
func brokerAuthenticate(w http.ResponseWriter, r *http.Request, kind brokerKind) *brokerCaller {
	endpoint := kind.endpoint
	p := peerFromContext(r.Context())
	switch classify(p) {
	case classAgent, classAgentUnknown, classUnconfirmed:
		// Both are fine here, and classAgentUnknown deliberately so: a
		// brokered SessionStart is often the event that first establishes
		// the conv-id, and a freshly spawned agent renders its status line
		// before that hook has. classUnconfirmed is also provisional here: a
		// version-named Claude process has no legacy harness-name match, but
		// the generation-bound pane proof below can still authenticate it.
		// Nothing is applied unless that proof or legacy identity succeeds.
	case classUnidentified:
		if checkBrokerRate(endpoint, brokerPreIdentityKey, brokerPreIdentityRatePerSecond).Reject {
			writeError(w, http.StatusTooManyRequests, "rate", "too many unplaceable requests")
			return nil
		}
		writeUnidentified(w)
		return nil
	default:
		writeUnconfirmed(w, r)
		return nil
	}
	if p.PID == 0 {
		writeUnidentified(w)
		return nil
	}

	// Resolve the ordinary candidate before parsing, but do not charge its
	// per-agent bucket yet: a reused pid can make this row provisional until
	// the request claim is checked against the live pane below. The shared,
	// high-capacity guard bounds pre-proof body parsing; the final row is charged
	// exactly once after identity settles.
	// One process-table view for the whole request: the resolution walk
	// here and the proof below read the same snapshot, so the proof cannot
	// lose a race against a caller that exits mid-request (see
	// broker_proc_table.go).
	timing := newBrokerTiming(endpoint, p.PID)
	placed := false
	defer func() {
		if !placed {
			timing.finish()
		}
	}()
	procs := newBrokerProcTable()
	row, harnessPID := hookSessionRowForPIDIn(procs, p.PID)
	timing.mark("resolve")
	if row != nil {
		timing.resolved = row.ID
	}
	preProofKey := brokerPreIdentityKey
	if row != nil {
		preProofKey = brokerPreIdentityKeyForRow(row.ID)
	}
	if checkBrokerRate(endpoint, preProofKey, brokerPreIdentityRatePerSecond).Reject {
		if row == nil {
			brokerRefusals.refuseUnplaceable(kind.noun+": caller could not be placed",
				brokerRefusalContext{Endpoint: endpoint, CallerPID: p.PID})
		}
		writeError(w, http.StatusTooManyRequests, "rate", "too many requests before identity verification")
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, brokerMaxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "body", "could not read request body")
		return nil
	}
	if len(body) > brokerMaxBody {
		resolvedID := ""
		if row != nil {
			resolvedID = row.ID
		}
		logBrokerBodyOverCap(endpoint, resolvedID, len(body))
		writeError(w, http.StatusRequestEntityTooLarge, "body", kind.noun+" payload too large")
		return nil
	}
	claimed, event, err := kind.decode(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "body", "malformed "+kind.noun+" payload")
		return nil
	}
	claimed = strings.TrimSpace(claimed)
	proofKey := ""
	if row != nil {
		proofKey = row.ID
	}
	if checkBrokerProofRate(endpoint, brokerProofKeyForRow(proofKey)).Reject {
		writeError(w, http.StatusTooManyRequests, "rate", "too many identity proof attempts")
		return nil
	}
	refusal := brokerRefusalContext{
		Endpoint: endpoint, CallerPID: p.PID, Claimed: claimed, Event: event,
		procs: procs, connAcceptedAt: connAcceptedAt(r.Context()), requestStartedAt: timing.start,
	}
	if row != nil {
		refusal.Resolved = row.ID
	}
	if !procs.exists(p.PID) {
		// The caller exited before the daemon got here — usually the
		// harness abandoning it, occasionally the client's own timeout.
		// Nothing can be delivered to it and a later callback takes its
		// place, so this is logged rather than badged as a refusal — the
		// badge means "this agent's telemetry is stuck", which a lost
		// callback is not. See noteCallerGone.
		brokerRefusals.noteCallerGone(refusal)
		writeError(w, http.StatusForbidden, "auth", "caller exited before its identity could be verified")
		return nil
	}
	timing.mark("parse")
	proof := proveTclaudeLayerCallerIn(procs, p.PID, claimed)
	timing.proof = &proof
	timing.mark("proof")
	proofDetail := proof.detail
	refusal.Detail = proofDetail
	refusal.claimedTmux = proof.claimedTmux
	layerClaim := proof.layerClaim
	switch {
	case proof.callerGone:
		brokerRefusals.noteCallerGone(refusal)
		writeError(w, http.StatusForbidden, "auth", "caller exited before its identity could be verified")
		return nil
	case layerClaim && proof.row != nil:
		row, harnessPID = proof.row, proof.harnessPID
		timing.resolved = row.ID
	case layerClaim && event == "SessionEnd" &&
		(proof.orphanTeardown || (sessionRecentlyShutDown(claimed) && !callerHasHarnessAncestor(procs, p.PID))):
		// The harness's last hook, orphaned: the harness fired SessionEnd on
		// its way out and exited without waiting, so the hook process was
		// reparented away from the pane (or the pane is already gone) and
		// cannot prove it. Recognised from the daemon's own record that the
		// claimed session is shutting down (session_shutdown.go), or failing
		// that from what the proof observed. Either way, only for a caller
		// with no harness above it — a live process in another agent's pane
		// has one. Still refused
		// (an orphan proves nothing), but this is ordinary teardown — the
		// pane exit observer records the exit itself — not a starved agent,
		// so it is neither badged nor warned about. Only a hook carries an
		// event, so a status line never lands here.
		brokerRefusals.noteOrphanedSessionEnd(refusal)
		writeError(w, http.StatusForbidden, "auth", "claimed tclaude’s sandbox session does not own this caller")
		return nil
	case layerClaim:
		if row != nil {
			brokerRefusals.refuseAttributed(
				kind.noun+": claimed tclaude’s sandbox session failed live-pane proof", refusal)
		} else {
			brokerRefusals.refuseUnplaceable(kind.noun+": tclaude’s sandbox caller failed live-pane proof", refusal)
		}
		writeError(w, http.StatusForbidden, "auth", "claimed tclaude’s sandbox session does not own this caller")
		return nil
	case row == nil:
		brokerRefusals.refuseUnplaceable(
			brokerClaimReason(kind.noun+": caller could not be placed", claimed, proofDetail), refusal)
		writeError(w, http.StatusForbidden, "auth",
			"could not resolve a session row for this caller; refusing to apply its "+kind.noun)
		return nil
	case isTclaudeLayerRow(row):
		// The resolved row IS a layer row, so this is very likely the
		// caller's own session; but a layer claim that did not PROVE is not
		// trusted to attribute, so it stays unplaceable. The log line still
		// carries the resolved row for the operator's correlation.
		brokerRefusals.refuseUnplaceable(
			brokerClaimReason(kind.noun+": tclaude’s sandbox callback omitted its session claim", claimed, proofDetail),
			refusal)
		writeError(w, http.StatusForbidden, "auth", kind.omittedClaimMessage)
		return nil
	case claimed != "" && claimed != row.ID:
		// Identity DID resolve here, so the refusal is attributed to the
		// row the DAEMON concluded — never to the claimed one, which is
		// the caller's own string. See broker_refusals.go.
		brokerRefusals.refuseAttributed(kind.noun+": claimed session id disagrees with the resolved row", refusal)
		writeError(w, http.StatusForbidden, "auth",
			"claimed session id does not match the session resolved for this caller")
		return nil
	}
	if checkBrokerRate(endpoint, row.ID, brokerRatePerSecond).Reject {
		writeError(w, http.StatusTooManyRequests, "rate", kind.rateMessage)
		return nil
	}
	placed = true
	return &brokerCaller{row: row, harnessPID: harnessPID, timing: timing}
}
