package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// shareTmux answers list-clients from a mutable client table and the window
// state from a status setting and a live size that resize-window updates, and
// records the detach/resize commands sharing issues.
type shareTmux struct {
	mu         sync.Mutex
	clients    []string // "tty\tflags\twidth\theight\twindow"
	status     string
	cols, rows int
	calls      [][]string
}

func (m *shareTmux) Command(args ...string) *exec.Cmd {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, append([]string(nil), args...))
	switch args[0] {
	case "list-clients":
		return exec.Command("printf", "%s", strings.Join(m.clients, "\n"))
	case "display-message":
		return exec.Command("printf", "%s", fmt.Sprintf("%s\t%d\t%d", m.status, m.cols, m.rows))
	case "resize-window":
		m.cols, _ = strconv.Atoi(args[4])
		m.rows, _ = strconv.Atoi(args[6])
	case "detach-client":
		if args[1] == "-t" {
			m.clients = slices.DeleteFunc(m.clients, func(c string) bool { return strings.HasPrefix(c, args[2]+"\t") })
		}
	}
	return exec.Command("true")
}
func (m *shareTmux) ListSessions() (map[string]struct{}, error) { return map[string]struct{}{}, nil }
func (m *shareTmux) commands(name string) [][]string {
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

func useShareTmux(t *testing.T, clients ...string) *shareTmux {
	t.Helper()
	m := &shareTmux{clients: clients, status: "on"}
	prev := clcommon.Default
	clcommon.Default = m
	t.Cleanup(func() { clcommon.Default = prev })
	return m
}

// shareView registers an incoming viewer rendering pin.
func shareView(t *testing.T, rt *fedRuntime, pin *fedPanePin) *fedTerminalView {
	t.Helper()
	v, err := rt.addTerminal("inst_peer", proto.SessionOpenPayload{Stream: proto.NewEnvelopeID(), Group: "team"}, true)
	require.NoError(t, err)
	v.mu.Lock()
	v.pin = pin
	v.mu.Unlock()
	t.Cleanup(v.close)
	return v
}

// sharingView registers a viewer that shares the size and could show cols x rows.
func sharingView(t *testing.T, rt *fedRuntime, pin *fedPanePin, cols, rows int) *fedTerminalView {
	t.Helper()
	v := shareView(t, rt, pin)
	v.mu.Lock()
	v.sharing = true
	v.sizeCh = make(chan struct{}, 1)
	v.mu.Unlock()
	v.setWant(cols, rows)
	return v
}

func TestFedLocalClientsSkipsRenderers(t *testing.T) {
	useShareTmux(t, "/dev/ttys001\tattached,focused,UTF-8\t200\t50\t@1", "/dev/ttys002\tattached,ignore-size,UTF-8\t80\t24\t@1", "garbage", "/dev/ttys003\tattached", "/dev/ttys004\tattached\t30\t10\t@2")
	clients, err := fedLocalClients("tclaude-x", "@1")
	require.NoError(t, err)
	require.Equal(t, []fedLocalClient{{tty: "/dev/ttys001", cols: 200, rows: 50}, {tty: "/dev/ttys003"}}, clients,
		"renderers attach with ignore-size and never count; a client without a reported size still does; one on another window does not")
	clients, err = fedLocalClients("tclaude-x", "")
	require.NoError(t, err)
	require.Len(t, clients, 3, "without a window, every local client counts (take-over detaches them all)")
}

func TestFedShareFloorsRemoteSizesOnly(t *testing.T) {
	v := &fedTerminalView{sharing: true}
	v.setWant(2, 1)
	require.Equal(t, [2]int{fedShareMinCols, fedShareMinRows}, [2]int{v.wantCols, v.wantRows}, "a remote viewer cannot shrink the window to a sliver")
	v.setWant(0, 0)
	require.Equal(t, [2]int{0, 0}, [2]int{v.wantCols, v.wantRows}, "no report stays no report")
	cols, rows := sharedFedSize([]*fedTerminalView{v}, []fedLocalClient{{cols: 10, rows: 3}})
	require.Equal(t, [2]int{10, 3}, [2]int{cols, rows}, "this node's own terminals are not floored")
}

func TestFedTerminalGroupsPreferTakeOver(t *testing.T) {
	cat := &proto.CatalogPayload{Groups: []proto.CatalogGroup{
		{Name: "a", Caps: []string{proto.CapSessionsWatch}},
		{Name: "b", Caps: []string{proto.CapSessionsWatch, proto.CapSessionsTakeover}},
		{Name: "c", Caps: []string{proto.CapSessionsTakeover}},
	}}
	var names []string
	for _, g := range fedTerminalGroups(cat) {
		names = append(names, g.Name)
	}
	require.Equal(t, []string{"b", "c", "a"}, names, "an open resolves to a take-over group whenever one shares the agent")
	require.Equal(t, "a", cat.Groups[0].Name, "the cached catalog is not reordered")
	require.False(t, fedGroupTakeover(cat.Groups[0]))
	require.True(t, fedGroupTakeover(cat.Groups[1]))
	require.False(t, fedGroupTakeover(cat.Groups[2]), "take-over without watch or attach opens nothing")
}

// TestFedWindowSizerLastLeaveWaits: once the last viewer's leave returns, no
// recompute is running or can start, so a later indicator restore is final.
func TestFedWindowSizerLastLeaveWaits(t *testing.T) {
	m := useShareTmux(t, "/dev/ttys001\tattached\t200\t50\t@1")
	rt := &fedRuntime{ctx: context.Background()}
	pin := &fedPanePin{tmux: "tclaude-x", pane: "%1", window: "@1", serverSession: "$1"}
	sharingView(t, rt, pin, 120, 40)
	s := rt.joinFedWindowSizer(pin)
	s.leave()
	select {
	case <-s.done:
	default:
		t.Fatal("the last leave returned before the recompute goroutine")
	}
	calls := len(m.commands("resize-window"))
	time.Sleep(3 * fedResizeSettle)
	require.Len(t, m.commands("resize-window"), calls, "nothing resizes the window after the last leave")
	fedSizersMu.Lock()
	defer fedSizersMu.Unlock()
	require.NotContains(t, fedSizers, s.key)
}

func TestPlanFedTakeOver(t *testing.T) {
	m := useShareTmux(t, "/dev/ttys002\tattached,ignore-size\t80\t24")
	rt := &fedRuntime{ctx: context.Background()}
	pin := &fedPanePin{tmux: "tclaude-x", window: "@1", serverSession: "$1"}
	v := shareView(t, rt, pin)
	shareView(t, rt, &fedPanePin{tmux: "tclaude-y", window: "@2", serverSession: "$2"})
	displace, others := rt.planFedTakeOver(pin, v, true)
	require.False(t, displace, "alone: nobody to displace, even when asked to; a viewer of another window is not involved")
	require.Empty(t, others)

	m.mu.Lock()
	m.clients = append(m.clients, "/dev/ttys001\tattached,focused\t200\t50")
	m.mu.Unlock()
	first := shareView(t, rt, pin)
	displace, others = rt.planFedTakeOver(pin, v, false)
	require.False(t, displace)
	require.Equal(t, []string{"a local terminal", "another remote viewer"}, others)
	displace, others = rt.planFedTakeOver(pin, v, true)
	require.True(t, displace)
	require.Empty(t, others, "a take-over will be alone")
	require.Empty(t, m.commands("detach-client"), "planning disconnects nobody")
	require.NoError(t, first.ctx.Err())
}

// TestFedTakeOverDisplaces: displacement closes the other viewers and detaches
// the local clients, including ones that attached since the plan, without
// waiting for the closed viewers to finish.
func TestFedTakeOverDisplaces(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()
	cleanupAgentdTestDB(t)
	m := useShareTmux(t, "/dev/ttys001\tattached,focused\t200\t50", "/dev/ttys002\tattached,ignore-size\t80\t24")
	rt := &fedRuntime{ctx: context.Background()}
	pin := &fedPanePin{tmux: "tclaude-x", window: "@1", serverSession: "$1"}
	other := shareView(t, rt, pin)
	v := shareView(t, rt, pin)
	m.mu.Lock()
	m.clients = append(m.clients, "/dev/ttys004\tattached\t100\t30")
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

func TestFedDebounceRunsOncePerBurst(t *testing.T) {
	var runs atomic.Int32
	d := newFedDebounce(30 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.run(ctx, func() { runs.Add(1) }); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	for range 20 {
		d.kick()
	}
	require.Eventually(t, func() bool { return runs.Load() == 1 }, time.Second, 5*time.Millisecond)
	time.Sleep(80 * time.Millisecond)
	require.EqualValues(t, 1, runs.Load(), "a burst of kicks runs once")
	d.kick()
	require.Eventually(t, func() bool { return runs.Load() == 2 }, time.Second, 5*time.Millisecond)
}

// TestFedWindowSizerTakesTheSmallestPerAxis: the window is the per-axis
// minimum of the sharing viewers and the local clients, less the status line;
// every sharing viewer renders the full shared size and learns what else is
// attached. A pinned (old) viewer counts as attached but does not size it.
func TestFedWindowSizerTakesTheSmallestPerAxis(t *testing.T) {
	m := useShareTmux(t, "/dev/ttys001\tattached\t200\t50\t@1", "/dev/ttys002\tattached,ignore-size\t10\t10\t@1", "/dev/ttys003\tattached\t10\t10\t@2")
	rt := &fedRuntime{ctx: context.Background()}
	pin := &fedPanePin{tmux: "tclaude-x", pane: "%1", window: "@1", serverSession: "$1"}
	wide := sharingView(t, rt, pin, 120, 60)
	tall := sharingView(t, rt, pin, 150, 30)
	shareView(t, rt, pin) // pinned: wants nothing
	s := &fedWindowSizer{rt: rt, pin: pin}
	s.apply()
	require.Equal(t, [][]string{{"resize-window", "-t", "@1", "-x", "120", "-y", "29"}}, m.commands("resize-window"))
	for _, v := range []*fedTerminalView{wide, tall} {
		require.Len(t, v.sizeCh, 1, "each sharing viewer is told")
		<-v.sizeCh
		cols, rows, status := v.sharedUpdate()
		require.Equal(t, [2]int{120, 30}, [2]int{cols, rows}, "the grid is the full shared size, status line included")
		var st proto.SessionStatus
		require.NoError(t, json.Unmarshal(status, &st))
		require.Equal(t, []string{"a local terminal", "2 other remote viewers"}, st.Others)
	}

	s.apply()
	require.Len(t, m.commands("resize-window"), 1, "an unchanged size is not re-applied")
	require.Empty(t, wide.sizeCh, "nothing changed for the viewers either")

	m.mu.Lock()
	m.cols, m.rows = 80, 24
	m.mu.Unlock()
	s.apply()
	require.Equal(t, []string{"resize-window", "-t", "@1", "-x", "120", "-y", "29"}, m.commands("resize-window")[1], "something else resized the window: the live size is compared, so it is restored")

	m.mu.Lock()
	m.clients = m.clients[1:]
	m.mu.Unlock()
	tall.setWant(150, 70)
	s.apply()
	require.Equal(t, []string{"resize-window", "-t", "@1", "-x", "120", "-y", "59"}, m.commands("resize-window")[2], "the local terminal left: the viewers alone size it")
	<-wide.sizeCh
	cols, rows, status := wide.sharedUpdate()
	require.Equal(t, [2]int{120, 60}, [2]int{cols, rows})
	var st proto.SessionStatus
	require.NoError(t, json.Unmarshal(status, &st))
	require.Equal(t, []string{"2 other remote viewers"}, st.Others)
	_, _, status = wide.sharedUpdate()
	require.Nil(t, status, "unchanged others are not re-sent")
}

func TestDetachTmuxSessionSparesRemoteRenderers(t *testing.T) {
	m := useShareTmux(t, "/dev/ttys001\tattached", "/dev/ttys002\tattached,ignore-size")
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
