package session

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/launchundo"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// fakeLaunchDaemon stands in for agentd's /v1/launch/op: it binds on the
// begin call and applies every later operation through the same
// ApplyBrokeredLaunchOp the daemon uses. It records what it was asked, which
// is how a test tells a brokered operation from a direct one — both end up
// in the same test database.
type fakeLaunchDaemon struct {
	mu     sync.Mutex
	ticket string
	bound  LaunchBinding
	ops    []string
}

func (d *fakeLaunchDaemon) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.ops...)
}

func (d *fakeLaunchDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req BrokeredLaunchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || r.URL.Path != LaunchOpPath {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if req.Ticket != d.ticket {
		http.Error(w, "unknown ticket", http.StatusForbidden)
		return
	}
	d.ops = append(d.ops, req.Op)
	if req.Op == LaunchBeginOp {
		if err := json.Unmarshal(req.Args, &d.bound); err != nil {
			http.Error(w, "bad binding", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(BrokeredLaunchResponse{})
		return
	}
	result, err := ApplyBrokeredLaunchOp(req.Op, req.Args, d.bound)
	switch {
	case errors.Is(err, ErrStaleLaunchGeneration):
		http.Error(w, "stale", http.StatusConflict)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		_ = json.NewEncoder(w).Encode(BrokeredLaunchResponse{Result: result})
	}
}

// brokerThisLaunch makes the test process a managed launcher talking to a
// fake daemon, exactly the way a real one becomes brokered: a ticket in the
// environment, adopted at the top of the launch.
func brokerThisLaunch(t *testing.T) *fakeLaunchDaemon {
	t.Helper()
	// A unix socket path has a short length limit, which TMPDIR can exceed.
	dir, err := os.MkdirTemp("/tmp", "tcl-launch-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agentd.sock")
	listener, err := net.Listen("unix", sock)
	require.NoError(t, err)
	daemon := &fakeLaunchDaemon{ticket: "ticket-for-this-launch"}
	server := &http.Server{Handler: daemon, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	t.Setenv(agentipc.SocketEnv, sock)
	t.Setenv(LaunchTicketEnvVar, daemon.ticket)
	t.Cleanup(func() { brokeredLaunch.Store(nil) })
	adoptLaunchTicket(true)
	require.True(t, LaunchIsBrokered())
	return daemon
}

// A managed launch that was handed a ticket sends its database work to the
// daemon instead of doing it, and the ticket never outlives the adoption in
// the launcher's environment.
func TestBrokeredLaunchRowGoesThroughTheDaemon(t *testing.T) {
	isolateCacheAndDB(t)
	daemon := brokerThisLaunch(t)
	_, inEnv := os.LookupEnv(LaunchTicketEnvVar)
	assert.False(t, inEnv, "the ticket must not stay in the environment tmux and the pane inherit")

	require.NoError(t, beginBrokeredLaunch("spwn-brokered", "gen-brokered"))
	var undo launchundo.Undo
	paneCommand := ""
	state, err := createLaunchRow(testLaunchRowInput("spwn-brokered", "gen-brokered", &paneCommand), &undo)
	require.NoError(t, err)

	saved, err := LoadSessionState("spwn-brokered")
	require.NoError(t, err)
	assert.Equal(t, "workspace-write", saved.HarnessBuiltinMode)
	assert.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin), saved.SandboxImplementation)
	identity, err := db.GetSessionExitLaunchIdentity("spwn-brokered")
	require.NoError(t, err)
	assert.Equal(t, "gen-brokered", identity.Generation)

	guard := disabledExitLaunchGuard("spwn-brokered", "spwn-brokered", "gen-brokered")
	guard.enabled = true
	state.PID = 4242
	_, err = opSaveLaunchedState.call(launchStateArgs{State: state})
	require.NoError(t, err)
	_, err = opExitMarkPending.call(launchGenerationOf("spwn-brokered", "gen-brokered"))
	require.NoError(t, err)
	guard.restorePreHarnessState()
	saved, err = LoadSessionState("spwn-brokered")
	require.NoError(t, err)
	assert.Equal(t, 4242, saved.PID)

	undo.Unwind()
	_, err = LoadSessionState("spwn-brokered")
	require.Error(t, err, "unwinding a brokered launch takes its row back out")

	assert.Equal(t, []string{
		LaunchBeginOp, opLaunchRowExists.name, opSaveLaunchRow.name,
		opSaveLaunchedState.name, opExitMarkPending.name, opExitMarkPending.name,
		opDeleteLaunchRow.name,
	}, daemon.seen(), "every database step of the launch was a daemon request")
}

// A launcher overtaken by a newer launch of the same session is told so, and
// its write does not land on the newer launch's row.
func TestBrokeredLaunchOpIsRefusedOnceSuperseded(t *testing.T) {
	isolateCacheAndDB(t)
	brokerThisLaunch(t)
	require.NoError(t, beginBrokeredLaunch("spwn-raced", "gen-old"))
	var undo launchundo.Undo
	paneCommand := ""
	state, err := createLaunchRow(testLaunchRowInput("spwn-raced", "gen-old", &paneCommand), &undo)
	require.NoError(t, err)

	// A newer launch takes the row over.
	newer := *state
	newer.PID = 9001
	require.NoError(t, SaveSessionStateForLaunch(&newer, "gen-new", db.SessionExitGateUngated))

	state.PID = 4242
	_, err = opSaveLaunchedState.call(launchStateArgs{State: state})
	require.ErrorIs(t, err, ErrStaleLaunchGeneration)
	_, err = opSeedModel.call(launchString("spwn-raced", "old-model"))
	require.ErrorIs(t, err, ErrStaleLaunchGeneration)

	// The old launch's rollback is conditional on its own generation.
	undo.Unwind()
	saved, err := LoadSessionState("spwn-raced")
	require.NoError(t, err, "the newer launch's row survives the older launch's rollback")
	assert.Equal(t, 9001, saved.PID)
}

// The daemon's half refuses anything outside the launch a ticket is bound to.
func TestApplyBrokeredLaunchOpFencesToItsLaunch(t *testing.T) {
	isolateCacheAndDB(t)
	bound := LaunchBinding{SessionID: "spwn-mine", Generation: "gen-mine"}
	args := func(v any) json.RawMessage {
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		return raw
	}
	mine := &SessionState{ID: "spwn-mine", Status: StatusIdle}
	theirs := &SessionState{ID: "spwn-theirs", Status: StatusIdle}
	require.NoError(t, SaveSessionStateForLaunch(theirs, "gen-theirs", db.SessionExitGateUngated))

	_, err := ApplyBrokeredLaunchOp(opSaveLaunchRow.name,
		args(launchStateArgs{State: theirs, Generation: "gen-mine"}), bound)
	require.ErrorIs(t, err, ErrLaunchOpForeignSession, "another session's row")
	_, err = ApplyBrokeredLaunchOp(opDeleteLaunchRow.name,
		args(launchGenerationOf("spwn-theirs", "gen-theirs")), bound)
	require.ErrorIs(t, err, ErrLaunchOpForeignSession, "another session's rollback")
	_, err = ApplyBrokeredLaunchOp(opSaveLaunchRow.name,
		args(launchStateArgs{State: mine, Generation: "gen-forged"}), bound)
	require.ErrorIs(t, err, ErrLaunchOpForeignSession, "a generation other than the bound one")
	_, err = ApplyBrokeredLaunchOp(opSeedModel.name, args(launchString("spwn-mine", "m")), bound)
	require.ErrorIs(t, err, ErrStaleLaunchGeneration, "no row carries the bound generation yet")
	_, err = ApplyBrokeredLaunchOp("session.drop_everything", args(launchSession("spwn-mine")), bound)
	require.ErrorIs(t, err, ErrUnknownLaunchOp)
	_, err = ApplyBrokeredLaunchOp(opLaunchRowExists.name, args(launchSession("spwn-mine")), LaunchBinding{})
	require.ErrorIs(t, err, ErrLaunchOpForeignSession, "an unbound ticket has no launch")

	untouched, err := LoadSessionState("spwn-theirs")
	require.NoError(t, err)
	assert.Equal(t, "spwn-theirs", untouched.ID)

	_, err = ApplyBrokeredLaunchOp(opSaveLaunchRow.name,
		args(launchStateArgs{State: mine, Generation: "gen-mine", GateState: db.SessionExitGateUngated}), bound)
	require.NoError(t, err)
	raw, err := ApplyBrokeredLaunchOp(opLaunchRowExists.name, args(launchSession("spwn-mine")), bound)
	require.NoError(t, err)
	assert.JSONEq(t, `{"exists":true}`, string(raw))
	_, err = ApplyBrokeredLaunchOp(opSeedModel.name, args(launchString("spwn-mine", "m")), bound)
	require.NoError(t, err, "fenced operations pass once the row carries the bound generation")
}

// Only a managed launch brokers, and the ticket is removed either way.
func TestAdoptLaunchTicketOnlyBrokersAManagedLaunch(t *testing.T) {
	t.Cleanup(func() { brokeredLaunch.Store(nil) })

	t.Setenv(LaunchTicketEnvVar, "inherited-by-accident")
	adoptLaunchTicket(false)
	assert.False(t, LaunchIsBrokered(), "a launch the daemon did not fork stays direct")
	_, inEnv := os.LookupEnv(LaunchTicketEnvVar)
	assert.False(t, inEnv)

	t.Setenv(LaunchTicketEnvVar, "")
	adoptLaunchTicket(true)
	assert.False(t, LaunchIsBrokered(), "a managed launch from a daemon that issues no ticket stays direct")
}

// A session state must survive the trip to the daemon: what the daemon
// writes for a brokered launch is the row the launcher would have written.
func TestSessionStateSurvivesTheBrokerWire(t *testing.T) {
	paneCommand := ""
	in := testLaunchRowInput("spwn-wire", "gen-wire", &paneCommand)
	isolateCacheAndDB(t)
	var undo launchundo.Undo
	state, err := createLaunchRow(in, &undo)
	require.NoError(t, err)
	state.PID = 77
	state.ResumeProvenance = "provenance"
	state.OSSandboxState, state.OSSandboxSource, state.OSSandboxUnverified = "on", "probe", true
	state.EffectiveSandbox = &sandboxpolicy.Snapshot{}

	raw, err := json.Marshal(launchStateArgs{State: state})
	require.NoError(t, err)
	var decoded launchStateArgs
	require.NoError(t, json.Unmarshal(raw, &decoded))
	want, got := toRow(state), toRow(decoded.State)
	// Instants keep their value, not their monotonic reading or location.
	for _, pair := range [][2]*time.Time{
		{&want.CreatedAt, &got.CreatedAt}, {&want.UpdatedAt, &got.UpdatedAt}, {&want.LastHook, &got.LastHook},
	} {
		assert.True(t, pair[0].Equal(*pair[1]))
		*pair[0], *pair[1] = time.Time{}, time.Time{}
	}
	assert.Equal(t, want, got)
}
