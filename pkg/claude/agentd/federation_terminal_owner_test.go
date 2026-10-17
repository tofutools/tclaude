package agentd

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// ownerTmux answers list-clients from a mutable client table and records the
// detach/resize commands a size claim issues.
type ownerTmux struct {
	mu      sync.Mutex
	clients []string // "tty\tflags"
	calls   [][]string
}

func (m *ownerTmux) Command(args ...string) *exec.Cmd {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, append([]string(nil), args...))
	switch args[0] {
	case "list-clients":
		return exec.Command("printf", "%s", strings.Join(m.clients, "\n"))
	case "detach-client":
		if args[1] == "-t" {
			m.clients = slices.DeleteFunc(m.clients, func(c string) bool { return strings.HasPrefix(c, args[2]+"\t") })
		}
	}
	return exec.Command("true")
}
func (m *ownerTmux) ListSessions() (map[string]struct{}, error) { return map[string]struct{}{}, nil }
func (m *ownerTmux) commands(name string) [][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out [][]string
	for _, c := range m.calls {
		if c[0] == name {
			out = append(out, c)
		}
	}
	return out
}

func useOwnerTmux(t *testing.T, clients ...string) *ownerTmux {
	t.Helper()
	m := &ownerTmux{clients: clients}
	prev := clcommon.Default
	clcommon.Default = m
	t.Cleanup(func() { clcommon.Default = prev })
	return m
}

// ownerView registers an incoming viewer rendering pin.
func ownerView(t *testing.T, rt *fedRuntime, pin *fedPanePin) *fedTerminalView {
	t.Helper()
	v, err := rt.addTerminal("inst_peer", proto.SessionOpenPayload{Stream: proto.NewEnvelopeID(), Group: "team"}, true)
	require.NoError(t, err)
	v.mu.Lock()
	v.pin = pin
	v.mu.Unlock()
	t.Cleanup(v.close)
	return v
}

func TestFedLocalClientsSkipsRenderers(t *testing.T) {
	useOwnerTmux(t, "/dev/ttys001\tattached,focused,UTF-8", "/dev/ttys002\tattached,ignore-size,UTF-8", "garbage", "/dev/ttys003\tattached")
	ttys, err := fedLocalClients("tclaude-x")
	require.NoError(t, err)
	require.Equal(t, []string{"/dev/ttys001", "/dev/ttys003"}, ttys)
}

func TestPlanFedSessionSizeAloneOwnsIt(t *testing.T) {
	// Only this viewer's own (or another session's) renderer is attached.
	m := useOwnerTmux(t, "/dev/ttys002\tattached,ignore-size")
	rt := &fedRuntime{ctx: context.Background()}
	pin := &fedPanePin{tmux: "tclaude-x", window: "@1", serverSession: "$1"}
	v := ownerView(t, rt, pin)
	elsewhere := ownerView(t, rt, &fedPanePin{tmux: "tclaude-y", window: "@2", serverSession: "$2"})
	require.False(t, rt.planFedSessionSize(pin, v, true), "alone: nobody to displace, even when asked to")
	require.True(t, v.sizeOwner())
	require.Empty(t, v.others)
	require.False(t, elsewhere.sizeOwner(), "a viewer of another window is not involved")
	require.Empty(t, m.commands("detach-client"))
}

func TestPlanFedSessionSizeSharedStaysPinned(t *testing.T) {
	m := useOwnerTmux(t, "/dev/ttys001\tattached,focused")
	rt := &fedRuntime{ctx: context.Background()}
	pin := &fedPanePin{tmux: "tclaude-x", window: "@1", serverSession: "$1"}
	first := ownerView(t, rt, pin)
	v := ownerView(t, rt, pin)
	require.False(t, rt.planFedSessionSize(pin, v, false))
	require.False(t, v.sizeOwner())
	require.Equal(t, []string{"a local terminal", "another remote viewer"}, v.others)
	require.Empty(t, m.commands("detach-client"), "without a take-over nobody is disconnected")
	require.NoError(t, first.ctx.Err())
}

// TestFedTakeOverPlansThenDisplaces: planning a take-over disconnects nobody
// (the open may still fail); displacement then closes the other viewers and
// detaches the local clients, including ones that attached since the plan,
// without waiting for the closed viewers to finish.
func TestFedTakeOverPlansThenDisplaces(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()
	cleanupAgentdTestDB(t)
	m := useOwnerTmux(t, "/dev/ttys001\tattached,focused", "/dev/ttys002\tattached,ignore-size")
	rt := &fedRuntime{ctx: context.Background()}
	pin := &fedPanePin{tmux: "tclaude-x", window: "@1", serverSession: "$1"}
	other := ownerView(t, rt, pin)
	v := ownerView(t, rt, pin)
	require.True(t, rt.planFedSessionSize(pin, v, true))
	require.True(t, v.sizeOwner(), "the take-over owns the size, as its answer says")
	require.Empty(t, v.others)
	require.Empty(t, m.commands("detach-client"), "planning disconnects nobody")
	require.NoError(t, other.ctx.Err())

	m.mu.Lock()
	m.clients = append(m.clients, "/dev/ttys004\tattached")
	m.mu.Unlock()
	rt.displaceFedClients(pin, v)
	require.ErrorIs(t, other.ctx.Err(), context.Canceled)
	other.mu.Lock()
	reason := other.reason
	other.mu.Unlock()
	require.Equal(t, fedTerminalTakenOver, reason, "the displaced viewer is told why")
	require.Equal(t, [][]string{{"detach-client", "-t", "/dev/ttys001"}, {"detach-client", "-t", "/dev/ttys004"}}, m.commands("detach-client"),
		"local clients are detached one by one; renderers are left to their viewers")
	require.NoError(t, v.ctx.Err(), "the taker itself stays")
	require.Empty(t, rt.fedWindowViewers(pin, v), "a displaced viewer no longer counts, even before its cleanup ran")
}

func TestFedSizeCoalescerAppliesOnlyTheLatest(t *testing.T) {
	var mu sync.Mutex
	var applied [][2]int
	c := newFedSizeCoalescer(30*time.Millisecond, func(cols, rows int) error {
		mu.Lock()
		defer mu.Unlock()
		applied = append(applied, [2]int{cols, rows})
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	for i := range 20 {
		c.set(100+i, 40)
	}
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(applied) == 1 }, time.Second, 5*time.Millisecond)
	c.set(119, 40)
	time.Sleep(100 * time.Millisecond)
	c.set(150, 50)
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(applied) == 2 }, time.Second, 5*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, [][2]int{{119, 40}, {150, 50}}, applied, "a burst applies once, at its latest size; an unchanged size is not re-applied")
}

func TestDetachTmuxSessionSparesRemoteRenderers(t *testing.T) {
	m := useOwnerTmux(t, "/dev/ttys001\tattached", "/dev/ttys002\tattached,ignore-size")
	fedRendererAdd("tclaude-x", 1)
	t.Cleanup(func() { fedRendererAdd("tclaude-x", -1) })
	detachTmuxSession("tclaude-x")
	require.Equal(t, [][]string{{"detach-client", "-t", "/dev/ttys001"}}, m.commands("detach-client"),
		"a local web terminal's teardown must not drop a remote viewer's renderer")
	require.False(t, fedRendererActive("tclaude-other"))
}

func TestSanitizeFedOthersBoundsPeerText(t *testing.T) {
	in := []string{"a local terminal", "", "\x1b[31mred", "one", "two", "three"}
	out := sanitizeFedOthers(in)
	require.Len(t, out, proto.MaxSessionOthers)
	require.Equal(t, "a local terminal", out[0])
	for _, s := range out {
		require.NotContains(t, s, "\x1b")
	}
}
