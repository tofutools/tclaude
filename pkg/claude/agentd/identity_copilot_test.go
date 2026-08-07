package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The process names a Copilot pane really presents on Linux, measured on the
// pinned 1.0.78 platform binary: the CLI is a Node SEA that renames its main
// thread, and /proc/<pid>/comm is the MAIN THREAD's name. So the binary at
// .../@github/copilot-linux-x64/copilot reads back as "MainThread", and the
// npm loader that spawned it reads back as "node-MainThread". Neither is a
// name the harness registry knows — while /proc/<pid>/exe, which the process
// cannot choose, names both correctly.
const (
	copilotSEAComm    = "MainThread"
	copilotLoaderComm = "node-MainThread"
)

// TestConvIDForPID_CopilotRenamedThreadStillResolves is the TCL-1049
// regression, at the layer that actually broke.
//
// The reported symptom was cosmetic — `tclaude agent whoami` answering
// "(unnamed)" for a Copilot agent the dashboard named correctly — but the
// cause was total: agentd could not identify a Copilot pane's caller AT ALL.
// The identity walk matched ancestors by process name, every process in a
// Copilot chain presents a renamed thread, so the walk reached init having
// matched nothing, classified the caller classUnconfirmed and refused it.
// whoami's empty answer printing "(unnamed)" was the mildest of the
// consequences; inbox, sends and replies were 403s.
//
// Both spawn topologies are pinned because they resolve through DIFFERENT
// probes, and a fix that only satisfied one would leave half the users broken.
func TestConvIDForPID_CopilotRenamedThreadStillResolves(t *testing.T) {
	// A human spawn with no tclaude layer: the pane `sh -c "… copilot"` is the
	// recorded pid and the loader is its direct child, so resolution lands on
	// the loader ancestor's PARENT — one hop above the match on the SEA.
	t.Run("plain pane", func(t *testing.T) {
		setupTestDB(t)

		const (
			peerPID    = 5101 // the `tclaude agent whoami` process over the socket
			toolShPID  = 5090 // the shell Copilot runs its tool commands in
			seaPID     = 5080 // the copilot binary — comm says "MainThread"
			loaderPID  = 5070 // the npm shim — comm says "node-MainThread"
			paneShPID  = 5060 // `sh -c "… copilot …"` = the recorded tmux pane pid
			convID     = "9a1e0000-0000-4000-8000-00000000c0d1"
			spawnLabel = "copilot-plain"
		)
		require.NoError(t, db.SaveSession(&db.SessionRow{
			ID:      spawnLabel,
			PID:     paneShPID,
			ConvID:  convID,
			Harness: harness.CopilotName,
			Status:  "working",
		}))

		fakeProcTree{
			name: map[int]string{
				peerPID: "tclaude", toolShPID: "bash",
				seaPID: copilotSEAComm, loaderPID: copilotLoaderComm, paneShPID: "sh",
			},
			exe: map[int]string{
				seaPID: harness.CopilotName, loaderPID: "node",
			},
			parent: map[int]int{
				peerPID: toolShPID, toolShPID: seaPID,
				seaPID: loaderPID, loaderPID: paneShPID,
			},
		}.install(t)

		gotConv, hasAncestor := convIDForPID(peerPID)
		assert.True(t, hasAncestor, "the copilot binary must be recognised by its executable")
		assert.Equal(t, convID, gotConv, "conv-id resolves via the loader ancestor's parent (pane sh) pid")
		assert.Equal(t, classAgent,
			classify(&peer{PID: peerPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor}))
	})

	// An agent-caller spawn, which for Copilot children is admitted in exactly
	// one topology: tclaude-layer. The bwrap + relay wrappers push the pane pid
	// several hops above the harness, so this one resolves through the
	// layer-ancestry probe instead. The chain is the one measured live on the
	// operator's machine.
	t.Run("tclaude layer", func(t *testing.T) {
		setupTestDB(t)

		const (
			peerPID   = 6101
			toolShPID = 6090
			seaPID    = 6080
			loaderPID = 6070
			bwrapPID  = 6060
			relayPID  = 6050
			outerPID  = 6040
			paneShPID = 6030
			convID    = "9a1e0000-0000-4000-8000-00000000c0d2"
		)
		require.NoError(t, db.SaveSession(&db.SessionRow{
			ID:                    "copilot-layer",
			PID:                   paneShPID,
			ConvID:                convID,
			Harness:               harness.CopilotName,
			SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
			Status:                "working",
		}))

		fakeProcTree{
			name: map[int]string{
				peerPID: "tclaude", toolShPID: "bash",
				seaPID: copilotSEAComm, loaderPID: copilotLoaderComm,
				bwrapPID: "bwrap", relayPID: "tclaude", outerPID: "tclaude", paneShPID: "bash",
			},
			exe: map[int]string{
				seaPID: harness.CopilotName, loaderPID: "node",
			},
			parent: map[int]int{
				peerPID: toolShPID, toolShPID: seaPID, seaPID: loaderPID,
				loaderPID: bwrapPID, bwrapPID: relayPID, relayPID: outerPID,
				outerPID: paneShPID,
			},
		}.install(t)

		gotConv, hasAncestor := convIDForPID(peerPID)
		assert.True(t, hasAncestor, "the copilot binary must be recognised by its executable")
		assert.Equal(t, convID, gotConv, "conv-id resolves through the tclaude-layer wrapper ancestry")
		assert.Equal(t, classAgent,
			classify(&peer{PID: peerPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor}))
	})

	// The bug itself, kept executable: with no executable evidence — a process
	// tree known only by its (renamed) names — the walk finds nothing and the
	// caller is refused as unconfirmed. This is what every Copilot pane looked
	// like to agentd before the fix, and it is what would come back if the
	// harness test were narrowed to names again.
	t.Run("name evidence alone still fails", func(t *testing.T) {
		setupTestDB(t)

		const (
			peerPID   = 7101
			toolShPID = 7090
			seaPID    = 7080
			loaderPID = 7070
			paneShPID = 7060
		)
		require.NoError(t, db.SaveSession(&db.SessionRow{
			ID:      "copilot-nameonly",
			PID:     paneShPID,
			ConvID:  "9a1e0000-0000-4000-8000-00000000c0d3",
			Harness: harness.CopilotName,
			Status:  "working",
		}))

		fakeProcTree{
			name: map[int]string{
				peerPID: "tclaude", toolShPID: "bash",
				seaPID: copilotSEAComm, loaderPID: copilotLoaderComm, paneShPID: "sh",
			},
			parent: map[int]int{
				peerPID: toolShPID, toolShPID: seaPID,
				seaPID: loaderPID, loaderPID: paneShPID,
			},
		}.install(t)

		gotConv, hasAncestor := convIDForPID(peerPID)
		assert.False(t, hasAncestor)
		assert.Empty(t, gotConv)
		assert.Equal(t, classUnconfirmed,
			classify(&peer{PID: peerPID, ConvID: gotConv, HasClaudeAncestor: hasAncestor}),
			"this is the refusal the operator hit: no harness ancestor, so not an agent at all")
	})
}

// A harness whose comm DOES name it keeps resolving through the name branch,
// with no executable evidence available at all. Claude Code (node), Codex and
// OpenCode are in that position on Linux today, which is why the bug was
// Copilot-only; this pins that the added evidence is a fallback and not a
// replacement.
func TestConvIDForPID_NamedHarnessResolvesWithoutExeEvidence(t *testing.T) {
	setupTestDB(t)

	const (
		peerPID   = 8201
		codexPID  = 8190
		paneShPID = 8180
		convID    = "9a1e0000-0000-4000-8000-00000000c0d4"
	)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:      "codex-named",
		PID:     paneShPID,
		ConvID:  convID,
		Harness: harness.CodexName,
		Status:  "working",
	}))

	fakeProcTree{
		name:   map[int]string{peerPID: "tclaude", codexPID: "codex", paneShPID: "sh"},
		parent: map[int]int{peerPID: codexPID, codexPID: paneShPID},
	}.install(t)

	gotConv, hasAncestor := convIDForPID(peerPID)
	assert.True(t, hasAncestor)
	assert.Equal(t, convID, gotConv)
}

// The harness-specific branch inside the walk keys on the harness NAME, so it
// has to see the name the executable established rather than the renamed
// thread. Pinned on OpenCode because it is the one branch that does: a
// packaged runtime reporting an unrelated comm must still take the
// server-authoritative probes.
func TestHarnessNameAt_ReportsTheHarnessNameNotTheThreadName(t *testing.T) {
	const (
		pid      = 9301
		plainPID = 9302
	)
	fakeProcTree{
		name: map[int]string{pid: "bun-MainThread", plainPID: "sh"},
		exe:  map[int]string{pid: harness.OpenCodeName, plainPID: "dash"},
	}.install(t)

	assert.Equal(t, harness.OpenCodeName, harnessNameAt(pid, procName(pid)))
	assert.Empty(t, harnessNameAt(plainPID, procName(plainPID)),
		"a pid whose name and exe both miss is not a harness")
}
