package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TCL-973. These drive the daemon's real spawn boundary rather than the
// catalog, so what they pin is the posture a Copilot pane would actually launch
// with — and, just as importantly, that this slice does NOT yet open detached
// Copilot spawning.

// A daemon-spawned Copilot pane is unattended, so an unchosen posture must
// resolve to the nonblocking default rather than to Copilot's own prompting
// one. `--allow-all-tools` and `--no-ask-user` are what that token renders, and
// both were measured on a PTY against the pinned 1.0.77 binary.
func TestCopilotSpawn_DefaultsToTheNonblockingPosture(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("copilot-crew")

	spawn := f.AsHuman().SpawnHarness("copilot-crew", "copilot-worker", harness.CopilotName)

	got, ok := f.World.SpawnApproval(spawn.ConvID)
	require.True(t, ok, "the copilot spawn should have been observed by the sim spawner")
	assert.Equal(t, harness.CopilotApprovalAllowTools, got,
		"an unattended Copilot pane must not default to Copilot's prompting posture")
}

// `inherit` stays selectable: an operator who wants their own Copilot
// configuration honoured verbatim asks for it, and it threads through unchanged
// rather than being quietly upgraded to the default.
func TestCopilotSpawn_InheritThreadsThroughUnchanged(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("copilot-crew")

	resp := f.AsHuman().SpawnWith("copilot-crew", map[string]any{
		"name":     "supervised",
		"harness":  harness.CopilotName,
		"approval": harness.CopilotApprovalInherit,
	})
	require.Equalf(t, http.StatusOK, resp.Code,
		"an explicit valid Copilot approval policy must be accepted; body=%s", resp.Raw)

	got, ok := f.World.SpawnApproval(resp.ConvID)
	require.True(t, ok, "the copilot spawn should have been observed by the sim spawner")
	assert.Equal(t, harness.CopilotApprovalInherit, got)
}

// The `yolo` token (TCL-1010) reaches the spawn boundary as a real posture, and
// the un-sandboxed pairing it creates is disclosed on the spawn RESPONSE rather
// than only in the dropdown's collapsed mode help. That channel is the point:
// `tclaude agent spawn` and a scripted POST both see the warning, and neither
// opens the dashboard's help affordance.
func TestCopilotSpawn_YoloIsAcceptedAndWarnsWithoutTheOuterLayer(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("copilot-crew")

	resp := f.AsHuman().SpawnWith("copilot-crew", map[string]any{
		"name":     "unbounded",
		"harness":  harness.CopilotName,
		"approval": harness.CopilotApprovalYolo,
	})
	require.Equalf(t, http.StatusOK, resp.Code,
		"yolo is a valid Copilot approval policy; body=%s", resp.Raw)

	got, ok := f.World.SpawnApproval(resp.ConvID)
	require.True(t, ok, "the copilot spawn should have been observed by the sim spawner")
	assert.Equal(t, harness.CopilotApprovalYolo, got)

	// The warning must name the posture that fixes it. A spawn response that
	// only said "this is dangerous" would leave the caller with nowhere to go.
	body := string(resp.Raw)
	assert.Contains(t, body, "--sandbox-impl tclaude-layer")
	assert.Contains(t, body, "not OS-confined")
	assert.Contains(t, body, "⚠")
}

// Another harness's token is a 400 at the boundary, not a launch that renders
// no permission flags while the row records an authority it never had.
func TestCopilotSpawn_ForeignApprovalTokensRejected(t *testing.T) {
	// `allow-all` stays foreign on purpose even though Copilot's own help calls
	// it the alias of the flag `yolo` renders: tclaude accepts one spelling per
	// posture, so a session row and the profile API cannot end up carrying two.
	for _, policy := range []string{"never", "auto", "allow-all", "plan"} {
		t.Run(policy, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("copilot-crew")

			resp := f.AsHuman().SpawnWith("copilot-crew", map[string]any{
				"name":     "bad-approval",
				"harness":  harness.CopilotName,
				"approval": policy,
			})
			require.Equalf(t, http.StatusBadRequest, resp.Code,
				"an invalid Copilot approval policy must be refused; body=%s", resp.Raw)
			assert.Contains(t, string(resp.Raw), "invalid_approval")
		})
	}
}

// TestCopilotSpawn_DetachedSpawningStaysRefused is the fail-closed statement
// this slice deliberately does not retract.
//
// The approval axis is now classifiable for Copilot in both directions, but the
// SANDBOX lineage matrix still has no Copilot arm, and it is consulted first. So
// an agent-to-agent Copilot spawn is refused as sandbox_restricted, in both
// directions, no matter how well-formed its approval posture is. That is the
// intended incremental seam: an approval catalog on its own must not become the
// change that opens detached Copilot agents, and this test fails loudly if some
// later change opens them without the sandbox arm landing too.
func TestCopilotSpawn_DetachedSpawningStaysRefused(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		parentHarness, parentPolicy string
		parentSandbox               string
		childHarness, childPolicy   string
		childSandbox                string
		wantStatus                  int
		wantCode                    string
		mustNotClaimApprovalRefusal bool
	}{
		{
			// A parent that provably holds full in-sandbox execution and could
			// therefore delegate `allow-tools` on the approval axis alone.
			name:          "capable claude parent cannot yet spawn a copilot child",
			parentHarness: harness.DefaultName, parentPolicy: "auto",
			parentSandbox: harness.ClaudeSandboxOff,
			childHarness:  harness.CopilotName, childPolicy: harness.CopilotApprovalAllowTools,
			childSandbox: harness.CopilotSandboxInherit,
			wantStatus:   http.StatusForbidden, wantCode: "sandbox_restricted",
			mustNotClaimApprovalRefusal: true,
		},
		{
			name:          "a copilot parent can spawn nothing yet",
			parentHarness: harness.CopilotName, parentPolicy: harness.CopilotApprovalAllowTools,
			parentSandbox: harness.CopilotSandboxInherit,
			childHarness:  harness.DefaultName, childPolicy: "plan",
			childSandbox: harness.ClaudeSandboxOn,
			wantStatus:   http.StatusForbidden, wantCode: "sandbox_restricted",
			mustNotClaimApprovalRefusal: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("alpha")
			const parent = "copilot-lineage-parent-aaaa-bbbb-cccccccc"
			haveSpawnCapableApprovalParent(t, f, "alpha", parent,
				tc.parentHarness, tc.parentPolicy, false)
			// haveSpawnCapableApprovalParent picks a sandbox mode per harness
			// and has no Copilot arm; set the row's mode explicitly so the
			// parent side of the sandbox matrix is the real Copilot one.
			row, err := db.FindSessionByConvID(parent)
			require.NoError(t, err)
			require.NotNil(t, row)
			row.HarnessBuiltinMode = tc.parentSandbox
			require.NoError(t, db.SaveSession(row))

			resp := f.AsAgent(parent).SpawnWith("alpha", map[string]any{
				"name":     "worker",
				"harness":  tc.childHarness,
				"sandbox":  tc.childSandbox,
				"approval": tc.childPolicy,
			})
			require.Equalf(t, tc.wantStatus, resp.Code, "spawn body=%s", resp.Raw)
			assert.Containsf(t, string(resp.Raw), tc.wantCode,
				"the refusal must name the gate that actually fired; body=%s", resp.Raw)
			if tc.mustNotClaimApprovalRefusal {
				assert.NotContainsf(t, string(resp.Raw), "approval_restricted",
					"approval is not what refuses this spawn today; body=%s", resp.Raw)
			}
		})
	}
}

// A Copilot row that predates this catalog carries a blank approval posture,
// which records no approval INPUT at all. Lifecycle repair therefore re-resolves
// it under current config (TCL-990) instead of pinning it to `inherit`, the
// value those launches historically resolved to. Pinning reproduced a
// resolution rather than an input, and left the relaunched agent at a posture
// approval lineage will not credit with in-sandbox authority — so it could not
// delegate.
func TestCopilotRelaunch_LegacyBlankRowReResolvesUnderCurrentConfig(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("copilot-crew")

	// Spawn a real Copilot agent so the clone has a live pane to work from,
	// then blank BOTH recorded postures — the session row and the durable
	// relaunch profile — to reproduce the pre-TCL-973 row shape, where no
	// Copilot launch recorded an approval policy because there was none to
	// record.
	spawn := f.AsHuman().SpawnHarness("copilot-crew", "legacy", harness.CopilotName)
	row, err := db.FindSessionByConvID(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, row)
	row.ApprovalPolicy = ""
	require.NoError(t, db.SaveSession(row))
	agentID, err := db.AgentIDForConv(spawn.ConvID)
	require.NoError(t, err)
	profile, err := db.AgentRelaunchProfileForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, profile)
	blank := ""
	profile.ApprovalPolicy = &blank
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, *profile))

	clone := f.AsHuman().CloneFresh(spawn.ConvID)
	require.Equalf(t, http.StatusOK, clone.Code, "clone body=%s", clone.Raw)

	got, ok := f.World.SpawnApproval(clone.NewConv)
	require.True(t, ok, "the clone should have been observed by the sim spawner")
	assert.Equal(t, harness.CopilotApprovalAllowTools, got,
		"a row that recorded no approval input re-resolves under current config (TCL-990), "+
			"rather than being pinned to the value it would historically have resolved to")
}
