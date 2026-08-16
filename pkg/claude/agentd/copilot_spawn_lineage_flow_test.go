package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Detached Copilot spawning, driven end to end through the production HTTP and
// executeSpawn boundaries (TCL-989 PR2).
//
// The unit matrix in spawn_sandbox_lineage_copilot_test.go decides WHO may ask.
// These flows are about whether the thing that gets asked for actually works:
// a Copilot pane that clears its trust modal, starts a turn, and comes back to
// idle, launched under tclaude's own wall with every directory it can write
// proven by the caller first.
//
// The failure mode they exist to catch is the silent one. A Copilot pane parked
// on a permission or trust dialog is running, healthy and answering tmux, and
// nothing in the daemon can tell that apart from an agent that is thinking —
// so "the spawn returned 200" is not evidence that anything happened.

const provenCopilotImplementation = string(sandboxpolicy.ImplementationTclaudeLayer)

// copilotLineageRemedyText is spelled out rather than built from the harness
// constants the production message uses, so a rename that quietly changes the
// sentence a caller reads fails here instead of agreeing with itself.
const copilotLineageRemedyText = "copilot agents are admitted in exactly one " +
	"launch topology — pass sandbox_implementation=tclaude-layer " +
	"(`--sandbox-impl tclaude-layer`; mode resolves to \"off\")"

// haveLineageParent writes a spawn-capable parent row carrying an explicit
// sandbox implementation and approval posture.
//
// It exists alongside haveSpawnCapableSandboxParent rather than replacing it
// because that helper writes SandboxImplementation:"" — which for Copilot is
// the legacy row that must be REFUSED. A Copilot-parent test built on it would
// pass for the wrong reason.
func haveLineageParent(
	t *testing.T, f *testharness.Flow, group, convID, h, sandbox, implementation, approval string,
) {
	t.Helper()
	f.HaveMember(group, convID)
	require.NoError(t, db.GrantAgentPermission(convID, agentd.PermGroupsMembersSpawn, "test"))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:                    "sess-" + convID,
		TmuxSession:           "tmux-" + convID,
		ConvID:                convID,
		Cwd:                   f.World.HomeDir,
		Status:                "running",
		Harness:               h,
		HarnessBuiltinMode:    sandbox,
		SandboxImplementation: implementation,
		ApprovalPolicy:        approval,
	}))
}

// haveProvenCopilotParent writes the one Copilot row that may spawn: the
// assert-off single wall, with the approval posture tclaude renders and records
// itself (`inherit` is unprovable and fails the approval gate on its own).
func haveProvenCopilotParent(t *testing.T, f *testharness.Flow, group, convID string) {
	t.Helper()
	haveLineageParent(t, f, group, convID, harness.CopilotName,
		harness.CopilotSandboxOff, provenCopilotImplementation,
		harness.CopilotApprovalAllowTools)
}

// siblingWorktree builds the layout defaultSiblingWorktreeTrust verifies: a
// real `git worktree add`ed sibling of a main worktree, which is what
// AddWorktreeIn picks by default and therefore the only layout an agent caller
// may pre-trust.
func siblingWorktree(t *testing.T) (repo, worktreeDir string) {
	t.Helper()
	repo, _ = initRepoOnMain(t)
	dir, err := worktree.AddWorktreeIn(repo, "agent-child", "main", "")
	require.NoError(t, err)
	return repo, dir
}

// TestCopilotLineage_AgentSpawnsProvenChildIntoVerifiedWorktree is the whole
// point of the PR, asserted on the surfaces that would be wrong if any single
// piece regressed.
//
// The caller is an AGENT, not a human: humans bypass both lineage gates, so a
// human-spawned variant of this test would prove nothing about either.
func TestCopilotLineage_AgentSpawnsProvenChildIntoVerifiedWorktree(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-cop1-aaaa-bbbb-cccc-111111111111"
	// Claude `inherit` is the TIGHTEST parent admitted to mint this child, so
	// it is the case that breaks first if the matrix arm is dropped.
	haveLineageParent(t, f, "alpha", parent, harness.DefaultName,
		harness.ClaudeSandboxInherit, "", "bypassPermissions")

	_, worktreeDir := siblingWorktree(t)

	// No trust_dir field: the pre-trust must come from the daemon's own
	// verification of the layout, not from the caller asking for it. An agent
	// that DID ask would be refused with trust_dir_restricted.
	resp := f.AsAgent(parent).SpawnWith("alpha", map[string]any{
		"name":                   "copilot-worker",
		"harness":                harness.CopilotName,
		"cwd":                    worktreeDir,
		"sandbox_implementation": provenCopilotImplementation,
		"initial_message":        "start work",
	})
	require.Equalf(t, http.StatusOK, resp.Code,
		"an admitted Copilot child must spawn rather than being refused as "+
			"sandbox_restricted / approval_restricted; body=%s", resp.Raw)

	// The row records the posture the guard judged and the launch built — the
	// same pair this child will later be read back as when it spawns children
	// of its own.
	row, err := db.FindSessionByConvID(resp.ConvID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, harness.CopilotSandboxOff, row.HarnessBuiltinMode)
	assert.Equal(t, provenCopilotImplementation, row.SandboxImplementation)

	// The launch really went through tclaude's own wall, wrapping the pane
	// directly the way Claude's and Codex's are — not OpenCode's server
	// boundary, and not a broad fallback.
	got, ok := f.World.SpawnSandboxImplementation(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, provenCopilotImplementation, got)
	assertSandboxLayerCalls(t, f, testharness.SandboxLayerInteractive)

	// The directory half: trust was auto-enabled from the verified layout, and
	// the repository grants were pinned rather than derived at launch time.
	trusted, ok := f.World.SpawnTrustDir(resp.ConvID)
	require.True(t, ok)
	assert.True(t, trusted,
		"a verified default sibling worktree must be pre-trusted with no opt-in")
	pinned, ok := f.World.SpawnGitWorktreeWriteDirs(resp.ConvID)
	require.True(t, ok)
	assert.NotEmpty(t, pinned,
		"a tclaude-layer Copilot child receives repository write paths, so the "+
			"daemon must pin the proven set rather than letting the launch derive it")

	// And the consequence that actually matters: the pane is not parked. This
	// is a real no-modal assertion — the simulator models Copilot's trust
	// dialog, and an un-trusted launch genuinely blocks here.
	sim := f.World.Copilots.GetByConvID(resp.ConvID)
	require.NotNil(t, sim)
	blocked, reason := sim.Blocked()
	require.Falsef(t, blocked, "the detached child must not park at the trust dialog: %s", reason)

	// Hooks are flowing and the agent completes a turn: working, then idle.
	assert.Equal(t, session.StatusWorking,
		copilotMember(t, f, "alpha", resp.ConvID).State.Status)
	sim.FinishTurn()
	sim.StartTurn("keep going")
	assert.Equal(t, testharness.CopilotToolAllowed, sim.RequestTool(testharness.CopilotToolCall{
		Kind: testharness.CopilotToolShell, Command: "go build ./..."}))
	sim.FinishTurn()
	assert.Equal(t, session.StatusIdle,
		copilotMember(t, f, "alpha", resp.ConvID).State.Status,
		"Stop must return the agent to idle, which a blocked pane can never do")
}

// TestCopilotLineage_ProvenParentOutboundMatrix drives the parent side through
// the real HTTP boundary: a Copilot agent spawning peers, including another
// Copilot.
func TestCopilotLineage_ProvenParentOutboundMatrix(t *testing.T) {
	for _, tc := range []struct {
		name           string
		childHarness   string
		childSandbox   string
		implementation string
		approval       string
		wantMode       string
	}{
		{
			name:           "copilot -> copilot",
			childHarness:   harness.CopilotName,
			implementation: provenCopilotImplementation,
			approval:       harness.CopilotApprovalAllowTools,
			wantMode:       harness.CopilotSandboxOff,
		},
		{
			name:         "copilot -> confined claude",
			childHarness: harness.DefaultName,
			childSandbox: harness.ClaudeSandboxOn,
			approval:     "default",
			wantMode:     harness.ClaudeSandboxOn,
		},
		{
			name:         "copilot -> codex read-only",
			childHarness: harness.CodexName,
			childSandbox: harness.SandboxReadOnly,
			approval:     harness.ApprovalNever,
			wantMode:     harness.SandboxReadOnly,
		},
		{
			name:         "copilot -> codex workspace-write",
			childHarness: harness.CodexName,
			childSandbox: harness.SandboxWorkspaceWrite,
			approval:     harness.ApprovalNever,
			wantMode:     harness.SandboxWorkspaceWrite,
		},
		{
			name:         "copilot -> codex managed profile",
			childHarness: harness.CodexName,
			childSandbox: harness.SandboxManagedProfile,
			approval:     harness.ApprovalNever,
			wantMode:     harness.SandboxManagedProfile,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCopilotFlow(t)
			f.HaveGroup("alpha")
			const parent = "parent-cop2-aaaa-bbbb-cccc-111111111111"
			haveProvenCopilotParent(t, f, "alpha", parent)

			body := map[string]any{
				"name":     "worker",
				"harness":  tc.childHarness,
				"approval": tc.approval,
			}
			if tc.childSandbox != "" {
				body["sandbox"] = tc.childSandbox
			}
			if tc.implementation != "" {
				body["sandbox_implementation"] = tc.implementation
			}
			resp := f.AsAgent(parent).SpawnWith("alpha", body)
			require.Equalf(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)

			row, err := db.FindSessionByConvID(resp.ConvID)
			require.NoError(t, err)
			require.NotNil(t, row)
			assert.Equal(t, tc.wantMode, row.HarnessBuiltinMode)
		})
	}
}

// TestCopilotLineage_RefusalTable is the negative half, and it asserts the
// KIND, not merely that something was refused.
//
// sandbox_restricted and approval_restricted are two independent gates with
// two different remedies, and a change that collapsed one into the other would
// still leave every "must be refused" assertion green.
//
// wantRemedy is the other half of that: for a Copilot child the KIND alone
// leaves the caller nowhere to go, because the admitted topology is a single
// cell the spawn defaults miss. The refusal must name the value to change on
// exactly the cases a caller can fix, and must NOT dangle it in front of a
// caller whose parent posture is what refused (TCL-993).
func TestCopilotLineage_RefusalTable(t *testing.T) {
	type parentSpec struct {
		harnessName    string
		sandbox        string
		implementation string
		approval       string
	}
	claudeInherit := parentSpec{harness.DefaultName, harness.ClaudeSandboxInherit, "", "bypassPermissions"}
	provenCopilot := parentSpec{harness.CopilotName, harness.CopilotSandboxOff,
		provenCopilotImplementation, harness.CopilotApprovalAllowTools}

	for _, tc := range []struct {
		name       string
		parent     parentSpec
		child      map[string]any
		wantCode   string
		wantRemedy bool
	}{
		{
			// The defaulted request: `tclaude agent spawn --harness copilot`
			// with nothing else spelled out. It is the shape a caller hits
			// first, and the one the remedy exists for.
			name:   "copilot child with no implementation is the legacy harness-builtin row",
			parent: claudeInherit,
			child: map[string]any{
				"harness":  harness.CopilotName,
				"approval": harness.CopilotApprovalAllowTools,
			},
			wantCode:   "sandbox_restricted",
			wantRemedy: true,
		},
		{
			// Refused one gate EARLIER than the lineage matrix, and the kind
			// says so. Copilot does ship an OS sandbox, but its modes only
			// assert that sandbox is off, so `harness-builtin` is a claim
			// tclaude cannot advertise for this harness at all — a malformed
			// value rather than a lineage verdict. Asserting the lineage kind
			// here would be asserting that this gate had been removed.
			name:   "copilot child pinning harness-builtin claims a wall tclaude has no lever for",
			parent: claudeInherit,
			child: map[string]any{
				"harness":                harness.CopilotName,
				"sandbox_implementation": string(sandboxpolicy.ImplementationHarnessBuiltin),
				"approval":               harness.CopilotApprovalAllowTools,
			},
			wantCode: "invalid_sandbox_implementation",
		},
		{
			name:   "copilot child at inherit leaves its posture to a settings file",
			parent: claudeInherit,
			child: map[string]any{
				"harness":  harness.CopilotName,
				"sandbox":  harness.CopilotSandboxInherit,
				"approval": harness.CopilotApprovalAllowTools,
			},
			wantCode: "sandbox_restricted",
			// Also fixable child-side: naming the implementation forces the
			// launch mode off, so the settings file stops deciding.
			wantRemedy: true,
		},
		{
			name:   "a codex workspace-write parent may not mint a tclaude-walled child",
			parent: parentSpec{harness.CodexName, harness.SandboxWorkspaceWrite, "", harness.ApprovalNever},
			child: map[string]any{
				"harness":                harness.CopilotName,
				"sandbox_implementation": provenCopilotImplementation,
				"approval":               harness.CopilotApprovalAllowTools,
			},
			wantCode: "sandbox_restricted",
		},
		{
			name:   "nor may a codex read-only parent",
			parent: parentSpec{harness.CodexName, harness.SandboxReadOnly, "", harness.ApprovalNever},
			child: map[string]any{
				"harness":                harness.CopilotName,
				"sandbox_implementation": provenCopilotImplementation,
				"approval":               harness.CopilotApprovalAllowTools,
			},
			wantCode: "sandbox_restricted",
		},
		{
			// The remedy must not be dangled here: this parent cannot mint the
			// admitted pair either, so a caller who followed it would be
			// refused a second time by the row directly above.
			name:   "a codex workspace-write parent gets no remedy for a defaulted copilot child",
			parent: parentSpec{harness.CodexName, harness.SandboxWorkspaceWrite, "", harness.ApprovalNever},
			child: map[string]any{
				"harness":  harness.CopilotName,
				"approval": harness.CopilotApprovalAllowTools,
			},
			wantCode:   "sandbox_restricted",
			wantRemedy: false,
		},
		{
			name: "nor does a legacy copilot parent, which cannot mint the admitted pair either",
			parent: parentSpec{harness.CopilotName, harness.CopilotSandboxOff,
				string(sandboxpolicy.ImplementationHarnessBuiltin), harness.CopilotApprovalAllowTools},
			child: map[string]any{
				"harness":  harness.CopilotName,
				"approval": harness.CopilotApprovalAllowTools,
			},
			wantCode:   "sandbox_restricted",
			wantRemedy: false,
		},
		{
			name: "a legacy copilot parent row asserts nothing about who owns its wall",
			parent: parentSpec{harness.CopilotName, harness.CopilotSandboxOff,
				string(sandboxpolicy.ImplementationHarnessBuiltin), harness.CopilotApprovalAllowTools},
			child: map[string]any{
				"harness":                harness.CopilotName,
				"sandbox_implementation": provenCopilotImplementation,
				"approval":               harness.CopilotApprovalAllowTools,
			},
			wantCode: "sandbox_restricted",
		},
		{
			name:   "a proven copilot parent may not mint a fully-open claude child",
			parent: provenCopilot,
			child: map[string]any{
				"harness":  harness.DefaultName,
				"sandbox":  harness.ClaudeSandboxOff,
				"approval": "default",
			},
			wantCode: "sandbox_restricted",
		},
		{
			name:   "nor a codex danger-full-access child",
			parent: provenCopilot,
			child: map[string]any{
				"harness":  harness.CodexName,
				"sandbox":  harness.SandboxDangerFull,
				"approval": harness.ApprovalNever,
			},
			wantCode: "sandbox_restricted",
		},
		{
			name:   "nor an opencode child, whose layer topology has no proven equivalence",
			parent: provenCopilot,
			child: map[string]any{
				"harness":  harness.OpenCodeName,
				"sandbox":  harness.OpenCodeSandboxAccessControl,
				"approval": harness.OpenCodeApprovalDeny,
			},
			wantCode: "sandbox_restricted",
		},
		{
			// The second, independent gate. The sandbox pair here is admitted;
			// the refusal is entirely about the posture the child asked for,
			// and a client that keyed on sandbox_restricted would render the
			// wrong remedy.
			name:   "an unprovable approval posture is refused by the OTHER gate",
			parent: parentSpec{harness.DefaultName, harness.ClaudeSandboxOn, "", "default"},
			child: map[string]any{
				"harness":                harness.CopilotName,
				"sandbox_implementation": provenCopilotImplementation,
				"approval":               harness.CopilotApprovalInherit,
			},
			wantCode: "approval_restricted",
		},
		{
			name:   "an unknown implementation is a malformed value, not a lineage verdict",
			parent: claudeInherit,
			child: map[string]any{
				"harness":                harness.CopilotName,
				"sandbox_implementation": "not-an-implementation",
				"approval":               harness.CopilotApprovalAllowTools,
			},
			wantCode: "invalid_sandbox_implementation",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCopilotFlow(t)
			f.HaveGroup("alpha")
			const parent = "parent-cop3-aaaa-bbbb-cccc-111111111111"
			haveLineageParent(t, f, "alpha", parent, tc.parent.harnessName,
				tc.parent.sandbox, tc.parent.implementation, tc.parent.approval)

			body := map[string]any{"name": "worker"}
			for k, v := range tc.child {
				body[k] = v
			}
			resp := f.AsAgent(parent).SpawnWith("alpha", body)
			require.NotEqualf(t, http.StatusOK, resp.Code, "must be refused; body=%s", resp.Raw)
			failure := decodeFailure(t, resp.Raw)
			assert.Equalf(t, tc.wantCode, failure.Code,
				"the refusal must name the gate that actually refused; body=%s", resp.Raw)
			if tc.wantRemedy {
				assert.Containsf(t, failure.Error, copilotLineageRemedyText,
					"a copilot refusal the caller can fix must name the value to change; body=%s", resp.Raw)
			} else {
				assert.NotContainsf(t, failure.Error, copilotLineageRemedyText,
					"this refusal is not fixable by naming a child-side implementation; body=%s", resp.Raw)
			}
		})
	}
}

// TestCopilotLineage_StackedIsNotTheProvenTopology keeps `stacked` out on the
// path an operator would actually reach it from. Nesting Copilot's own
// experimental MXC policy inside tclaude's produces an effective confinement
// nobody reviewed while the row would name one wall.
func TestCopilotLineage_StackedIsNotTheProvenTopology(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-cop4-aaaa-bbbb-cccc-111111111111"
	haveLineageParent(t, f, "alpha", parent, harness.DefaultName,
		harness.ClaudeSandboxOff, "", "bypassPermissions")

	resp := f.AsAgent(parent).SpawnWith("alpha", map[string]any{
		"name":                   "worker",
		"harness":                harness.CopilotName,
		"sandbox_implementation": string(sandboxpolicy.ImplementationStacked),
		"approval":               harness.CopilotApprovalAllowTools,
	})
	require.NotEqualf(t, http.StatusOK, resp.Code,
		"stacked is not the reviewed Copilot topology; body=%s", resp.Raw)
}

// TestCopilotLineage_TemplatePathCarriesTheImplementation covers the OTHER
// spawn boundary. Templates, waves and process adapters never pass through the
// HTTP request path, so executeSpawn re-runs the guard itself — and the
// implementation has to reach it from the profile rather than from a request
// body that does not exist here.
func TestCopilotLineage_TemplatePathCarriesTheImplementation(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-cop5-aaaa-bbbb-cccc-111111111111"
	haveLineageParent(t, f, "alpha", parent, harness.DefaultName,
		harness.ClaudeSandboxInherit, "", "bypassPermissions")
	require.NoError(t, db.GrantAgentPermission(parent, agentd.PermTemplatesUse, "test"))

	createBody := map[string]any{
		"name": "copilot-template",
		"agents": []map[string]any{{
			"name":     "worker",
			"harness":  harness.CopilotName,
			"approval": harness.CopilotApprovalAllowTools,
			"profile_inline": map[string]any{
				"harness":                harness.CopilotName,
				"sandbox_implementation": provenCopilotImplementation,
			},
		}},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := agentReqProof(t, f, parent, http.MethodPost,
		"/v1/templates/copilot-template/instantiate",
		map[string]any{"group_name": "copilot-cast", "cwd": t.TempDir()})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equalf(t, 1, res.Spawned,
		"the Copilot child must be admitted through executeSpawn: %+v", res.Agents)
	require.Len(t, res.Agents, 1)

	row, err := db.FindSessionByConvID(res.Agents[0].ConvID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, harness.CopilotSandboxOff, row.HarnessBuiltinMode,
		"the template path must persist the forced single-wall mode too")
	assert.Equal(t, provenCopilotImplementation, row.SandboxImplementation)
}

// TestCopilotLineage_ProofCoversRepositoryWritePaths is the regression test for
// the write-permission escape the admission would otherwise have opened.
//
// A tclaude-layer launch derives its repository grants from the Claude `on`
// shape whatever the harness, so a Copilot child's outer wall really does get
// the repository container and the checkout's exact Git dir writable. The
// challenge must therefore name them: a proof that covered only cwd would let a
// parent confined to one worktree write every sibling repository through the
// child.
func TestCopilotLineage_ProofCoversRepositoryWritePaths(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-cop6-aaaa-bbbb-cccc-111111111111"
	haveLineageParent(t, f, "alpha", parent, harness.DefaultName,
		harness.ClaudeSandboxInherit, "", "bypassPermissions")

	repo, worktreeDir := siblingWorktree(t)
	body := map[string]any{
		"name":                   "copilot-worker",
		"harness":                harness.CopilotName,
		"cwd":                    worktreeDir,
		"sandbox_implementation": provenCopilotImplementation,
		"approval":               harness.CopilotApprovalAllowTools,
	}

	rec := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body)
	ch := decodeWriteProofChallenge(t, rec)

	gitDir, err := harness.GitDir(worktreeDir)
	require.NoError(t, err)
	assert.Containsf(t, ch.WriteProof.Dirs, filepath.Dir(repo),
		"the challenge must name the repository container the outer wall makes "+
			"writable, not only cwd; dirs=%v", ch.WriteProof.Dirs)
	assert.Containsf(t, ch.WriteProof.Dirs, gitDir,
		"the challenge must name the checkout's exact Git dir; dirs=%v", ch.WriteProof.Dirs)

	// Proving cwd alone is exactly the bypass: it must be refused, and refused
	// as a failed proof rather than as a lineage verdict.
	cwdOnly := filepath.Join(worktreeDir, ch.WriteProof.Filename)
	require.NoError(t, os.WriteFile(cwdOnly, nil, 0o600))
	t.Cleanup(func() { _ = os.Remove(cwdOnly) })
	partial := make(map[string]any, len(body)+1)
	for k, v := range body {
		partial[k] = v
	}
	partial["write_proof_token"] = ch.WriteProof.Token
	refused := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", partial)
	require.Equalf(t, http.StatusForbidden, refused.Code,
		"a cwd-only proof must not admit the spawn; body=%s", refused.Body.String())
	assert.Contains(t, refused.Body.String(), "write_proof_failed")

	// The full proof succeeds, and the dirs the caller proved are the dirs the
	// launch was pinned to.
	full := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", body)
	fullCh := decodeWriteProofChallenge(t, full)
	answerChallenge(t, fullCh)
	retry := make(map[string]any, len(body)+1)
	for k, v := range body {
		retry[k] = v
	}
	retry["write_proof_token"] = fullCh.WriteProof.Token
	ok := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", retry)
	require.Equalf(t, http.StatusOK, ok.Code, "proved spawn; body=%s", ok.Body.String())

	var spawned struct {
		ConvID string `json:"conv_id"`
	}
	testharness.DecodeJSON(t, ok, &spawned)
	pinned, found := f.World.SpawnGitWorktreeWriteDirs(spawned.ConvID)
	require.True(t, found)
	assert.Containsf(t, pinned, gitDir,
		"the launch must receive the daemon-pinned grants, not ones it derived "+
			"for itself after the proof; pinned=%v", pinned)
	for _, dir := range pinned {
		assert.Containsf(t, fullCh.WriteProof.Dirs, dir,
			"every pinned grant must be one the caller actually proved")
	}
}

// TestCopilotLineage_ProvenParentIsNotProofExempt guards the other side of the
// same pairing. A Copilot row at `off` under tclaude-layer is CONFINED, so
// "completing" the fully-open exemption list with it would hand every Copilot
// agent an unproven write anywhere.
func TestCopilotLineage_ProvenParentIsNotProofExempt(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("alpha")
	const parent = "parent-cop7-aaaa-bbbb-cccc-111111111111"
	haveProvenCopilotParent(t, f, "alpha", parent)

	rec := agentReq(t, f, parent, http.MethodPost, "/v1/groups/alpha/spawn", map[string]any{
		"name":     "worker",
		"harness":  harness.DefaultName,
		"sandbox":  harness.ClaudeSandboxOn,
		"cwd":      t.TempDir(),
		"approval": "default",
	})
	ch := decodeWriteProofChallenge(t, rec)
	require.NotEmpty(t, ch.WriteProof.Dirs,
		"a confined Copilot parent must be challenged like any other confined caller")
}

// TestCopilotLineage_TemporaryUnlockRevokesSpawnAuthority closes the gap the
// implementation column exists to close.
//
// The dashboard's temporary sandbox unlock stands tclaude's wall down for one
// process launch. For Claude and Codex the MODE string changes with it, so the
// lineage guard sees the new posture either way. For Copilot the mode stays
// `off` in both states — the column is the only thing separating a confined
// agent from an unconfined one — so this asserts the relaunch really projects
// harness-builtin onto the LIVE session row the guard reads, rather than
// leaving a stale tclaude-layer claim behind.
func TestCopilotLineage_TemporaryUnlockRevokesSpawnAuthority(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("alpha")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":                   "copilot-parent",
		"harness":                harness.CopilotName,
		"sandbox_implementation": provenCopilotImplementation,
		"approval":               harness.CopilotApprovalAllowTools,
		"trust_dir":              true,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "source spawn; body=%s", spawn.Raw)
	require.NoError(t, db.GrantAgentPermission(spawn.ConvID, agentd.PermGroupsMembersSpawn, "test"))

	// While confined, it may mint the proven child.
	admitted := f.AsAgent(spawn.ConvID).SpawnWith("alpha", map[string]any{
		"name":                   "worker-before",
		"harness":                harness.CopilotName,
		"sandbox_implementation": provenCopilotImplementation,
		"approval":               harness.CopilotApprovalAllowTools,
	})
	require.Equalf(t, http.StatusOK, admitted.Code,
		"a confined Copilot parent may mint the proven child; body=%s", admitted.Raw)

	// The operator's own dashboard control, not a poke at the database: the
	// projection under test is the one that endpoint actually performs.
	f.SetSessionStatus(spawn.ConvID, "idle")
	unlock := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
		testharness.JSONRequest(t, http.MethodPost,
			"/api/agents/"+spawn.ConvID+"/sandbox-restart",
			map[string]any{"action": "unlock"}))
	require.Equalf(t, http.StatusOK, unlock.Code, "unlock body=%s", unlock.Body.String())

	row, err := db.FindSessionByConvID(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin),
		row.SandboxImplementation,
		"an unlocked relaunch must rewrite the live row's implementation; the mode "+
			"string does not change for Copilot, so this column is the only evidence")

	refused := f.AsAgent(spawn.ConvID).SpawnWith("alpha", map[string]any{
		"name":                   "worker-after",
		"harness":                harness.CopilotName,
		"sandbox_implementation": provenCopilotImplementation,
		"approval":               harness.CopilotApprovalAllowTools,
	})
	require.Equalf(t, http.StatusForbidden, refused.Code,
		"an unlocked Copilot parent is unconfined and may mint nothing; body=%s", refused.Raw)
	assert.Equal(t, "sandbox_restricted", decodeFailure(t, refused.Raw).Code)
}
