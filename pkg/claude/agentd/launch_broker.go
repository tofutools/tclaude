package agentd

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

// --- /v1/launch/op ---
//
// The agentd side of brokered launch operations (pkg/claude/session
// launch_ops.go). The `tclaude session new` that agentd forks for a managed
// launch does not write the database: it sends each database step here, and
// the daemon runs the same function the launcher would have run. That is what
// lets a launcher run where there is no database — another host, a pod.
//
// Who may call. Nothing the launcher can show about its process identifies
// it: it is a short-lived child that exists before its session has a pane,
// so there is no pane ancestry to prove, and on a remote runner there will be
// no peer credential either. So the daemon gives each launch a ticket, a
// random bearer secret handed to the one process it forks, and that ticket is
// the launch's whole authority:
//
//   - it is handed over on a pipe, never in argv or the environment, which
//     other processes of the same user can read;
//   - it is minted for the session the daemon is launching, and bound by the
//     launcher's first call to that session and one launch generation; it
//     can never be re-bound;
//   - every operation must name that session, an operation that carries a
//     generation must carry that one, one that names a conversation must
//     name the one the daemon is resuming or told it to create, and one that
//     names a Codex app-server runtime must name the one the daemon started
//     for this launch;
//   - before the bind it serves only the few reads the launcher needs to
//     settle its identity, and only for the session it was minted for;
//   - it is revoked when the launcher process exits, and expires regardless.
//
// A ticket therefore cannot be used to report for another session, which is
// the same guarantee the hook broker gives a pane: a launch can only speak
// for itself. What a ticket holder CAN do is exactly what the launcher did by
// writing the database directly, so brokering takes a capability away from
// the launcher (reaching the database) and adds none.
//
// Fencing. Operations of one session are serialized here, and an operation
// registered as fenced is refused with 409 once the session row carries a
// different generation: a launcher overtaken by a newer launch of the same
// session must not finish its bookkeeping on the newer launch's row. The
// refusal is logged; it is the launcher's to report.

// launchTicketTTL is a backstop. A ticket is revoked when its launcher exits;
// this only bounds the life of one whose launcher the daemon lost track of.
const launchTicketTTL = time.Hour

// launchTicketBytes is the size of a ticket before hex encoding.
const launchTicketBytes = 32

// launchTicketScope is what the daemon knows about the launch it mints a
// ticket for. None of it is the launcher's to say; operations are checked
// against it.
type launchTicketScope struct {
	// sessionID is the session being launched, or "" when the launcher
	// derives it (a fresh launch given neither label nor conversation).
	sessionID string
	// convID is the conversation the launch resumes, or "".
	convID string
	// presetConvID is the conversation id chosen for a fresh launch, or "".
	presetConvID string
	// codexAppServerGeneration is the Codex app-server runtime started for
	// this launch, or "".
	codexAppServerGeneration string
}

type launchTicket struct {
	mu      sync.Mutex
	scope   launchTicketScope
	bound   session.LaunchBinding
	expires time.Time
}

var launchTickets = struct {
	sync.Mutex
	byHash map[[sha256.Size]byte]*launchTicket
}{byHash: make(map[[sha256.Size]byte]*launchTicket)}

// launchOpLocks serializes the brokered operations of one session, so the
// generation an operation was fenced against is the generation it runs
// against. Striped rather than keyed: the set of session ids is unbounded and
// two sessions sharing a stripe only costs them a moment of ordering.
var launchOpLocks [64]sync.Mutex

func launchOpLock(sessionID string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(sessionID))
	return &launchOpLocks[h.Sum32()%uint32(len(launchOpLocks))]
}

// mintLaunchTicket issues the ticket for the launch scope describes. The
// daemon keeps only its hash. revoke is idempotent.
func mintLaunchTicket(scope launchTicketScope) (ticket string, revoke func(), err error) {
	var raw [launchTicketBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, err
	}
	ticket = hex.EncodeToString(raw[:])
	key := sha256.Sum256([]byte(ticket))
	now := time.Now()
	launchTickets.Lock()
	for k, t := range launchTickets.byHash {
		if now.After(t.expires) {
			delete(launchTickets.byHash, k)
		}
	}
	launchTickets.byHash[key] = &launchTicket{scope: scope, expires: now.Add(launchTicketTTL)}
	launchTickets.Unlock()
	return ticket, func() {
		launchTickets.Lock()
		delete(launchTickets.byHash, key)
		launchTickets.Unlock()
	}, nil
}

func lookupLaunchTicket(ticket string) *launchTicket {
	ticket = strings.TrimSpace(ticket)
	if len(ticket) != 2*launchTicketBytes {
		return nil
	}
	key := sha256.Sum256([]byte(ticket))
	launchTickets.Lock()
	defer launchTickets.Unlock()
	t := launchTickets.byHash[key]
	if t == nil || time.Now().After(t.expires) {
		delete(launchTickets.byHash, key)
		return nil
	}
	return t
}

// validLaunchBindingField bounds what a launcher may bind a ticket to. The
// values become database keys and log fields, never paths or commands.
func validLaunchBindingField(v string) bool {
	if v == "" || len(v) > 256 || v != strings.TrimSpace(v) {
		return false
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// bind attaches the ticket to its launch. The first binding wins; repeating
// it is harmless, and any other is refused.
func (t *launchTicket) bind(b session.LaunchBinding) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.scope.sessionID != "" && b.SessionID != t.scope.sessionID {
		return false
	}
	b.CodexAppServerGeneration, b.ConvID, b.PresetConvID = "", "", ""
	if t.bound == (session.LaunchBinding{}) {
		t.bound = b
		return true
	}
	return t.bound == b
}

// binding is the launch the ticket serves. Before the launcher has bound it,
// that is only the session it was minted for, which is all an operation that
// may run before the bind is checked against.
func (t *launchTicket) binding() session.LaunchBinding {
	t.mu.Lock()
	defer t.mu.Unlock()
	bound := t.bound
	if bound.SessionID == "" {
		bound.SessionID = t.scope.sessionID
	}
	bound.CodexAppServerGeneration = t.scope.codexAppServerGeneration
	bound.ConvID, bound.PresetConvID = t.scope.convID, t.scope.presetConvID
	return bound
}

// handleLaunchOp runs one database operation on behalf of a launch.
func handleLaunchOp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	// The daemon's own socket only: a launcher is a local child today, and a
	// remote one will arrive over the runner channel, not a network listener.
	if p := peerFromContext(r.Context()); p == nil || p.PID == 0 {
		writeUnidentified(w)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, brokerMaxBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "body", "could not read request body")
		return
	}
	if len(body) > brokerMaxBody {
		writeError(w, http.StatusRequestEntityTooLarge, "body", "launch operation payload too large")
		return
	}
	var req session.BrokeredLaunchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "body", "malformed launch operation payload")
		return
	}
	ticket := lookupLaunchTicket(req.Ticket)
	if ticket == nil {
		writeError(w, http.StatusForbidden, "ticket", "unknown, expired or revoked launch ticket")
		return
	}
	if req.Op == session.LaunchBeginOp {
		var binding session.LaunchBinding
		if err := json.Unmarshal(req.Args, &binding); err != nil ||
			!validLaunchBindingField(binding.SessionID) || !validLaunchBindingField(binding.Generation) {
			writeError(w, http.StatusBadRequest, "body", "launch binding requires a session id and a generation")
			return
		}
		if !ticket.bind(binding) {
			slog.Warn("launch broker: ticket is already bound to another launch",
				"session", binding.SessionID, "module", "launch")
			writeError(w, http.StatusForbidden, "ticket", "launch ticket is bound to another launch")
			return
		}
		ops, err := json.Marshal(session.LaunchBeginResult{Ops: session.LaunchOpNames()})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "launch", "could not encode the launch operation list")
			return
		}
		writeJSON(w, http.StatusOK, session.BrokeredLaunchResponse{Result: ops})
		return
	}
	// An unbound ticket gets through to the registry, which serves it only
	// the operations that may run before the bind.
	bound := ticket.binding()
	lock := launchOpLock(bound.SessionID)
	lock.Lock()
	// A launcher that gave up while this waited has started unwinding;
	// applying its operation now would write behind its rollback.
	if r.Context().Err() != nil {
		lock.Unlock()
		return
	}
	result, err := session.ApplyBrokeredLaunchOp(req.Op, req.Args, bound)
	lock.Unlock()
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, session.BrokeredLaunchResponse{Result: result})
	case errors.Is(err, session.ErrStaleLaunchGeneration):
		slog.Warn("launch broker: dropped an operation from a superseded launch",
			"session", bound.SessionID, "generation", bound.Generation, "op", req.Op, "module", "launch")
		writeError(w, http.StatusConflict, "stale_launch", "this launch is no longer the session's current launch")
	case errors.Is(err, session.ErrLaunchOpForeignSession):
		slog.Warn("launch broker: refused an operation outside the ticket's launch",
			"session", bound.SessionID, "op", req.Op, "module", "launch")
		writeError(w, http.StatusForbidden, "ticket", "operation does not belong to this ticket's launch")
	case errors.Is(err, session.ErrUnknownLaunchOp):
		// 501 so the launcher can tell "this daemon is older than me" from a
		// malformed request.
		writeError(w, http.StatusNotImplemented, "op", "unknown launch operation")
	default:
		// The caller is the daemon's own launcher and reports this on the
		// stderr the daemon captures, so the cause travels with it.
		slog.Warn("launch broker: operation failed",
			"session", bound.SessionID, "op", req.Op, "error", err, "module", "launch")
		writeError(w, http.StatusInternalServerError, "launch", err.Error())
	}
}
