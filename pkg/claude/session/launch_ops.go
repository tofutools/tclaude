package session

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Launch operations: the database work a launch does, defined once.
//
// `tclaude session new` used to write the tclaude database itself. That only
// works while the launcher runs on the daemon's host with the daemon's
// filesystem; a runner on another host, or in a pod, has no database to
// write. So every database step of a launch is a launchOp: a name and one
// function. Where it RUNS is the only thing that varies:
//
//   - direct: in this process. This is what the daemon itself uses, and what
//     an unmanaged launch uses — a human running `tclaude session new`,
//     `tclaude conv` resuming a conversation, `tclaude task run`. Those are
//     host processes of the operator with no daemon-issued identity, and
//     some of them (`--no-daemon`) run with no daemon at all.
//   - brokered: the launcher POSTs the op's name and arguments to agentd,
//     which runs the SAME function. This is what a managed launch (one
//     agentd forked, `--managed-launch`) uses, authenticated by the launch
//     ticket below, from the moment the launch binds that ticket.
//
// One function per op, run on either side, is the point: there is no second
// implementation that has to be kept in agreement with the first.
//
// Fencing. A brokered launch is bound to its session id and launch
// generation, and an op for any other session is refused. An op marked
// fenceCurrentGeneration is additionally refused once the session row carries
// a different generation, so a launcher that was overtaken by a newer launch
// of the same session cannot finish its bookkeeping on the newer launch's
// row. Ops whose statement is already conditional on the generation carry it
// in their arguments instead, and it must be the bound one.

// LaunchTicketFDEnvVar names the inherited descriptor agentd wrote the launch
// ticket to for the `tclaude session new` it forks. The ticket is a bearer
// secret for one launch, so it travels over a pipe: argv and the environment
// are both readable by other processes of the same user for as long as the
// launcher lives (/proc/<pid>/cmdline, /proc/<pid>/environ — unsetting a
// variable does not change the latter). See adoptLaunchTicket.
const LaunchTicketFDEnvVar = "TCLAUDE_LAUNCH_TICKET_FD"

// launchTicketHexLen is the length of a ticket: 32 random bytes, hex encoded.
const launchTicketHexLen = 64

// LaunchOpPath is agentd's endpoint for brokered launch operations.
const LaunchOpPath = "/v1/launch/op"

// LaunchBeginOp binds a ticket to the launch it will serve. It is not a
// database operation, so it is not in the registry; agentd handles it.
const LaunchBeginOp = "launch.begin"

// launchOpTimeout bounds one brokered launch operation. A launch is not on a
// harness's critical path, but the operator is waiting for it; the daemon it
// talks to forked it moments ago, so anything slower than this is a fault to
// report rather than a delay to sit through.
const launchOpTimeout = 15 * time.Second

var (
	// ErrStaleLaunchGeneration reports that the session row now belongs to a
	// different launch than the one asking.
	ErrStaleLaunchGeneration = errors.New("launch generation is no longer current for this session")
	// ErrLaunchOpForeignSession reports an op naming a session, or a
	// generation, other than the one its ticket is bound to.
	ErrLaunchOpForeignSession = errors.New("launch operation does not belong to this launch")
	// ErrUnknownLaunchOp reports an op name the registry does not hold.
	ErrUnknownLaunchOp = errors.New("unknown launch operation")
)

// LaunchBinding is the launch a ticket serves.
type LaunchBinding struct {
	SessionID  string `json:"session_id"`
	Generation string `json:"generation"`
}

// BrokeredLaunchRequest is the body of a brokered launch operation.
type BrokeredLaunchRequest struct {
	Ticket string          `json:"ticket"`
	Op     string          `json:"op"`
	Args   json.RawMessage `json:"args"`
}

// BrokeredLaunchResponse carries an operation's result back.
type BrokeredLaunchResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
}

// LaunchBeginResult is what agentd answers LaunchBeginOp with: the
// operations it can run. The launcher and the daemon are separate binaries
// and are not always the same version — the daemon keeps running across an
// upgrade of the tclaude on PATH — so the launcher checks an operation
// against this list and fails with a restart hint instead of a bare refusal.
type LaunchBeginResult struct {
	Ops []string `json:"ops"`
}

// LaunchOpNames lists the registered operations, for LaunchBeginResult.
func LaunchOpNames() []string {
	names := make([]string, 0, len(launchOpRegistry))
	for name := range launchOpRegistry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// launchFence says how the daemon fences a brokered op.
type launchFence int

const (
	// fenceSessionOnly: the op must name the bound session. Used by the op
	// that establishes the generation, by reads, and by ops whose statement
	// is conditional on the generation they carry.
	fenceSessionOnly launchFence = iota
	// fenceCurrentGeneration: additionally, the session row must still carry
	// the bound generation.
	fenceCurrentGeneration
)

// launchOpArgs is what every op's arguments expose so the daemon can fence
// the op without knowing its shape.
type launchOpArgs interface {
	// launchSessionID is the session the op touches.
	launchSessionID() string
	// launchGeneration is the generation the op's own statement is
	// conditional on, or "" when it carries none.
	launchGeneration() string
}

type registeredLaunchOp struct {
	apply func(raw json.RawMessage, bound LaunchBinding) (any, error)
}

var launchOpRegistry = map[string]registeredLaunchOp{}

// launchOp is one database operation of a launch.
type launchOp[A launchOpArgs, R any] struct {
	name string
	run  func(A) (R, error)
}

// defineLaunchOp registers an op. Called from package-level initializers
// only, so the registry is complete before anything can call into it.
func defineLaunchOp[A launchOpArgs, R any](name string, fence launchFence, run func(A) (R, error)) launchOp[A, R] {
	if _, dup := launchOpRegistry[name]; dup || name == LaunchBeginOp {
		panic("duplicate launch op " + name)
	}
	launchOpRegistry[name] = registeredLaunchOp{
		apply: func(raw json.RawMessage, bound LaunchBinding) (any, error) {
			var args A
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, fmt.Errorf("decode %s arguments: %w", name, err)
			}
			if args.launchSessionID() != bound.SessionID {
				return nil, ErrLaunchOpForeignSession
			}
			if g := args.launchGeneration(); g != "" && g != bound.Generation {
				return nil, ErrLaunchOpForeignSession
			}
			if fence == fenceCurrentGeneration {
				if err := requireCurrentLaunchGeneration(bound); err != nil {
					return nil, err
				}
			}
			return run(args)
		},
	}
	return launchOp[A, R]{name: name, run: run}
}

// requireCurrentLaunchGeneration refuses when the session row is gone or
// belongs to another launch.
//
// agentd serializes the brokered operations of one session, so no other
// brokered launch can change the answer under the op that follows. Processes
// that still write the row directly — an unmanaged launch of the same label —
// are outside that lock, so against those this is a check, not a transaction;
// the statements that matter most are conditional on the generation as well.
func requireCurrentLaunchGeneration(bound LaunchBinding) error {
	identity, err := db.GetSessionExitLaunchIdentity(bound.SessionID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrStaleLaunchGeneration
	case err != nil:
		// A database fault is not a superseded launch; report it as itself.
		return fmt.Errorf("read launch generation: %w", err)
	case identity.Generation != bound.Generation:
		return ErrStaleLaunchGeneration
	}
	return nil
}

// call runs the op: in this process, or in agentd when this launch is
// brokered.
func (op launchOp[A, R]) call(args A) (R, error) {
	broker := brokeredLaunch.Load()
	if broker == nil {
		return op.run(args)
	}
	var result R
	err := broker.post(op.name, args, &result)
	return result, err
}

// ApplyBrokeredLaunchOp runs one brokered operation on behalf of the launch
// bound holds. It is agentd's half of launchOp.call; the caller has already
// authenticated the ticket and holds the session's operation lock.
func ApplyBrokeredLaunchOp(name string, raw json.RawMessage, bound LaunchBinding) (json.RawMessage, error) {
	op, ok := launchOpRegistry[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownLaunchOp, name)
	}
	if bound.SessionID == "" || bound.Generation == "" {
		return nil, ErrLaunchOpForeignSession
	}
	result, err := op.apply(raw, bound)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

// launchBroker is the brokered side's client state: the one ticket this
// process was started with, and what the daemon it reached can do.
type launchBroker struct {
	ticket string
	ops    map[string]bool
}

// pendingLaunchTicket holds an adopted ticket until the launch binds it.
var pendingLaunchTicket atomic.Pointer[string]

// brokeredLaunch is nil in every process except a managed
// `tclaude session new`, which sets it once before it does anything else.
var brokeredLaunch atomic.Pointer[launchBroker]

// LaunchIsBrokered reports whether this process hands its launch operations
// to agentd.
func LaunchIsBrokered() bool { return brokeredLaunch.Load() != nil }

// adoptLaunchTicket is the first thing a launch does: it takes the launch
// ticket off the descriptor agentd handed over and closes that descriptor, so
// nothing this process starts — tmux, and through a freshly started tmux
// server every pane it ever creates — can inherit either.
//
// The ticket is only kept for a managed launch. It becomes this process's
// authority when the launch binds it (beginBrokeredLaunch); a launch that
// never binds, such as the lightweight shell path, stays direct.
//
// A managed launch without a ticket stays direct too. That is what a tclaude
// newer than its daemon looks like for the length of an upgrade, and it is
// exactly the behaviour the older daemon expects.
func adoptLaunchTicket(managed bool) {
	raw := strings.TrimSpace(os.Getenv(LaunchTicketFDEnvVar))
	_ = os.Unsetenv(LaunchTicketFDEnvVar)
	pendingLaunchTicket.Store(nil)
	if raw == "" {
		return
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return
	}
	file := os.NewFile(uintptr(fd), "launch-ticket")
	if file == nil {
		return
	}
	// The daemon wrote the ticket and closed its end before it forked, so
	// this read ends at once; the limit only bounds a descriptor that is not
	// what the variable said it was.
	data, _ := io.ReadAll(io.LimitReader(file, launchTicketHexLen+1))
	_ = file.Close()
	ticket := strings.TrimSpace(string(data))
	if !managed || len(ticket) != launchTicketHexLen {
		return
	}
	if _, err := hex.DecodeString(ticket); err != nil {
		return
	}
	pendingLaunchTicket.Store(&ticket)
}

// beginBrokeredLaunch binds this process's ticket to its session and launch
// generation and switches the process to brokered operations. Every later
// operation is fenced against that binding. A launch with no ticket has
// nothing to bind and stays direct.
func beginBrokeredLaunch(sessionID, generation string) error {
	ticket := pendingLaunchTicket.Swap(nil)
	if ticket == nil {
		return nil
	}
	broker := &launchBroker{ticket: *ticket}
	var begun LaunchBeginResult
	if err := broker.post(LaunchBeginOp, LaunchBinding{SessionID: sessionID, Generation: generation}, &begun); err != nil {
		return fmt.Errorf("register launch with agentd: %w", err)
	}
	broker.ops = make(map[string]bool, len(begun.Ops))
	for _, name := range begun.Ops {
		broker.ops[name] = true
	}
	brokeredLaunch.Store(broker)
	return nil
}

func (b *launchBroker) post(op string, args, result any) error {
	if op != LaunchBeginOp && !b.ops[op] {
		return fmt.Errorf("%s: the running agentd does not know this launch operation; "+
			"it is older than this tclaude — restart agentd", op)
	}
	rawArgs, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("encode %s arguments: %w", op, err)
	}
	body, err := json.Marshal(BrokeredLaunchRequest{Ticket: b.ticket, Op: op, Args: rawArgs})
	if err != nil {
		return fmt.Errorf("encode %s request: %w", op, err)
	}
	var out BrokeredLaunchResponse
	if err := agentipc.PostToBroker(LaunchOpPath, body, launchOpTimeout, &out); err != nil {
		if statusErr, ok := errors.AsType[*agentipc.BrokerStatusError](err); ok && statusErr.Status == http.StatusConflict {
			return fmt.Errorf("%s: %w (%w)", op, ErrStaleLaunchGeneration, err)
		}
		return fmt.Errorf("%s: %w", op, err)
	}
	if result == nil || len(out.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(out.Result, result); err != nil {
		return fmt.Errorf("decode %s result: %w", op, err)
	}
	return nil
}
