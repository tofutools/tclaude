//go:build rewire

package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/GiGurra/rewire/pkg/rewire"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TestSpawnFlow_RenamesAndResumes exercises the spawn → /rename →
// resume sequence. It guards the bug class where each piece works in
// isolation but the daemon, the post-init injector goroutine, and
// the resume handler don't agree on the conv-id / tmux session
// lifecycle (the silent "resumed" lie was the canonical example).
//
// Lives in `package agentd_test` because rewire's scanner resolves
// rewire.Func target args through their `pkg.Name` form, and an
// internal test (package agentd) provides no package qualifier for
// bare identifiers. Trade-off: the mocked spawn helpers had to be
// renamed to exported (SpawnDetachedTclaudeNew / Resume).
//
// Mocks installed here (rewire's scanner only walks _test.go files,
// so every rewire.Func call must live in one):
//   - clcommon.TmuxCommand → testharness.FakeTmux.Command: returns a
//     no-op `true` cmd so injectTextAndSubmit's send-keys succeed;
//     records each invocation; flips has-session exit on the fake
//     alive table.
//   - agentd.SpawnDetachedTclaudeNew → MaterializeSpawn: writes a
//     session row keyed by `label` with a synthesised conv-id +
//     tmux session + marks tmux alive, all synchronously, so the
//     spawn handler's LoadSession poll loop succeeds on its first
//     iteration.
//   - agentd.SpawnDetachedTclaudeResume → no-op recorder: confirms
//     the resume path actually shells out without paying for a real
//     subprocess.
func TestSpawnFlow_RenamesAndResumes(t *testing.T) {
	w := testharness.New(t)
	mux := agentd.BuildHandlerForTest()

	rewire.Func(t, clcommon.TmuxCommand, w.Tmux.Command)
	rewire.Func(t, agentd.SpawnDetachedTclaudeNew, func(label, cwd string) error {
		w.CC.MaterializeSpawn(t, label, cwd)
		return nil
	})
	resumeCalls := 0
	rewire.Func(t, agentd.SpawnDetachedTclaudeResume, func(_, _ string) error {
		resumeCalls++
		return nil
	})

	if _, err := db.CreateAgentGroup("alpha", ""); err != nil {
		t.Fatalf("CreateAgentGroup: %v", err)
	}

	rec := testharness.Serve(mux, agentd.AsHumanPeer(testharness.JSONRequest(t,
		"POST", "/v1/groups/alpha/spawn",
		map[string]any{"alias": "worker"})))
	if rec.Code != http.StatusOK {
		t.Fatalf("spawn status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var spawn struct {
		Group       string `json:"group"`
		ConvID      string `json:"conv_id"`
		Label       string `json:"label"`
		TmuxSession string `json:"tmux_session"`
	}
	testharness.DecodeJSON(t, rec, &spawn)
	if spawn.ConvID == "" || spawn.TmuxSession == "" {
		t.Fatalf("spawn response missing conv_id/tmux_session: %+v", spawn)
	}

	// The post-init goroutine in handleGroupSpawn should now inject
	// `/rename worker`. Its waitForConvAlive loop polls every 500ms,
	// then sleeps 1s once alive, then injectTextAndSubmit fires
	// send-keys with two 500ms pauses. ~2.5s upper bound — give 5s.
	target := spawn.TmuxSession + ":0.0"
	if !w.Tmux.WaitForSendKeys(target, "/rename worker", 5*time.Second) {
		t.Fatalf("post-init did not inject /rename worker into %s; sent=%+v",
			target, w.Tmux.Sent())
	}

	// Mark the conv offline so resume actually exercises the spawn
	// path (not the "skipped:already_online" short-circuit).
	w.Tmux.MarkOffline(spawn.TmuxSession)

	rec = testharness.Serve(mux, agentd.AsHumanPeer(testharness.JSONRequest(t,
		"POST", "/v1/agent/"+spawn.ConvID+"/resume", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("resume status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resume struct {
		ConvID string `json:"conv_id"`
		Action string `json:"action"`
	}
	testharness.DecodeJSON(t, rec, &resume)
	if resume.Action != "resumed" {
		t.Errorf("resume action = %q, want %q (body=%s)",
			resume.Action, "resumed", rec.Body.String())
	}
	if resumeCalls != 1 {
		t.Errorf("SpawnDetachedTclaudeResume called %d times, want 1", resumeCalls)
	}
}
