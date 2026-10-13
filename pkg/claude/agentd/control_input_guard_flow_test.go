package agentd_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestControlInput_HumanDialogsRefuseLifecycle(t *testing.T) {
	for _, name := range []string{harness.DefaultName, harness.PiName} {
		t.Run(name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("crew")
			conv := "019ec011-1111-2222-3333-444444444444"
			if name == harness.PiName {
				t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
				resp, sim := spawnPi(t, f, "crew", map[string]any{"name": "pi-worker", "approval": "ask"})
				conv = resp.ConvID
				require.Eventually(t, func() bool { return sim.Name() == "pi-worker" }, 3*time.Second, time.Millisecond)
				require.NoError(t, sim.OpenDialog("confirm", "Allow bash?"))
			} else {
				f.HaveAliveSession(conv, "worker", "guard-worker", "/work")
				f.HaveMember("crew", conv)
			}
			for _, status := range []string{session.StatusAwaitingPermission, session.StatusAwaitingInput} {
				f.SetSessionStatus(conv, status)
				before := len(f.World.Tmux.Sent())
				rename := f.AsHuman().Rename(conv, "safe-name")
				require.Equal(t, http.StatusConflict, rename.Code, string(rename.Raw))
				require.Contains(t, string(rename.Raw), "busy")
				compact := f.AsHuman().CompactWithFollowUp(conv, "continue work")
				require.Equal(t, http.StatusServiceUnavailable, compact.Code, string(compact.Raw))
				require.Contains(t, string(compact.Raw), "busy")
				require.Len(t, f.World.Tmux.Sent(), before, "no text, paste or Enter may reach an open dialog")
			}
			f.SetSessionStatus(conv, session.StatusIdle)
			rename := f.AsHuman().Rename(conv, "safe-name")
			require.Equal(t, http.StatusOK, rename.Code, string(rename.Raw))
			compact := f.AsHuman().CompactWithFollowUp(conv, "continue work")
			require.Equal(t, http.StatusOK, compact.Code, string(compact.Raw))
		})
	}
}

func TestControlInput_SpawnRenameWaitsForHuman(t *testing.T) {
	f := newFlow(t)
	const conv = "019ec012-1111-2222-3333-444444444444"
	f.HaveAliveSession(conv, "worker", "guard-worker", "/work")
	f.SetSessionStatus(conv, session.StatusAwaitingPermission)
	waiting, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	t.Cleanup(agentd.SetPaneInputWaitForTest(func() { once.Do(func() { close(waiting) }); <-release }))
	before := len(f.World.Tmux.Sent())
	go func() { agentd.RunSpawnPostInitForTest(conv, "held-name"); close(done) }()
	select {
	case <-waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("spawn rename never reached its held retry")
	}
	require.Len(t, f.World.Tmux.Sent(), before, "spawn rename cannot answer the approval dialog")
	f.SetSessionStatus(conv, session.StatusIdle)
	unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("spawn rename did not resume after the human answered")
	}
	require.Equal(t, "held-name", f.World.CCs.GetByConvID(conv).Title())
}
