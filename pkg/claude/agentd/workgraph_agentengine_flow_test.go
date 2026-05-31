package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// JOH-15 B1 — the agent-as-engine seam. These flow tests stand in for a REAL
// agent-driver (B2): a mock group-owner caller drives an `engine: agent` instance
// over /v1 while the daemon tick runs, proving the per-pass gating contract:
//
//   - the daemon STILL mechanically runs tool/program nodes (F1(i)),
//   - the daemon does NOT auto-spawn ai nodes (the driver does, via the new
//     spawn-into-node verb),
//   - the daemon's safety guards (max_visits) still fire, and
//   - the instance reaches a terminal state driven by the owner's /v1 calls.

// Scenario (headline): a group-owner driver takes an engine:agent instance to
// completion via /v1. The daemon runs the mechanical tool nodes and suppresses
// its ai-spawn pass; the driver spawns the worker into the ready ai node and
// settles it; the daemon then runs the trailing tool node and the instance
// completes.
func TestWorkgraphEngine_AgentModeDriverDrivesToCompletion(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))

	// A group + a driver conv that OWNS it. The engine:agent driver's authority is
	// plain group-ownership (F2) — the same authority that already settles any node
	// — so this introduces no new authz surface.
	g := f.HaveGroup("squad")
	const driver = "drvr-aaaa-bbbb-cccc-9999"
	f.HaveConvWithTitle(driver, "engine-driver")
	require.NoError(t, db.AddAgentGroupOwner(g.ID, driver, "human"), "AddAgentGroupOwner")

	// A graph with BOTH a mechanical tool node (the daemon must still run it in
	// agent mode) and an ai node (the daemon must NOT auto-spawn — the driver
	// does): prep(tool) -> work(ai) -> done(tool).
	root := t.TempDir()
	writeToolTemplate(t, root, "agentdrive",
		"name: agentdrive\nengine: agent\nentry: prep\n",
		"flowchart TD\n prep --> work\n work --> done\n",
		map[string]string{
			"prep": "executor:\n  kind: tool\n  run: echo prepped\n",
			"work": "executor:\n  kind: ai\n  agent: worker\n  prompt: do the work\n",
			"done": "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	id := v1Create(t, f, "project:agentdrive", "squad")

	// engine_mode is snapshotted as agent at create (from the template's engine:).
	inst, _ := db.GetWorkgraphInstance(id)
	require.NotNil(t, inst)
	require.Equal(t, "agent", inst.EngineMode, "engine_mode snapshotted from the template engine: field")

	// One tick: the daemon runs the mechanical prep(tool) node and readies work,
	// but in agent mode it must NOT auto-spawn the ai node.
	agentd.RunWorkgraphEngineTickForTest()

	prep, _ := db.GetWorkgraphNode(id, "prep")
	assert.Equal(t, "done", prep.Status, "the daemon STILL runs the mechanical tool node in agent mode")
	work, _ := db.GetWorkgraphNode(id, "work")
	require.Equal(t, "ready", work.Status, "the ai node is left READY — the daemon did NOT auto-spawn it in agent mode")
	assert.Empty(t, work.Assignee, "no assignee — no daemon auto-spawn happened")

	// Belt-and-suspenders: the suppressed auto-spawn would have logged a
	// node_started event on the ai node — there must be none yet.
	wEvents, _ := db.ListWorkgraphEvents(id, "work")
	for _, e := range wEvents {
		assert.NotEqual(t, db.WorkgraphEventNodeStarted, e.Kind, "the daemon must not have started the ai node")
	}

	// The DRIVER (group-owner) spawns a worker into the ready ai node via /v1 — the
	// new spawn-into-node verb. This is the judgment the daemon delegated.
	startPath := "/v1/workgraphs/" + strconv.FormatInt(id, 10) + "/nodes/work/start"
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, startPath, nil), driver))
	require.Equal(t, http.StatusOK, rec.Code, "driver spawns into the ai node; body=%s", rec.Body.String())
	var startResp struct {
		Status   string `json:"status"`
		Assignee string `json:"assignee"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &startResp))
	assert.Equal(t, "running", startResp.Status)
	require.NotEmpty(t, startResp.Assignee, "spawned worker conv recorded as the node assignee")

	// The driver-spawned worker is a member of the bound group.
	var joined bool
	for _, m := range f.ListGroupMembers("squad") {
		if m.ConvID == startResp.Assignee {
			joined = true
		}
	}
	assert.True(t, joined, "the driver-spawned worker %q joined the bound group", startResp.Assignee)

	// The driver settles the ai node done via the node-PATCH gate — graph-level
	// drive authority as the group-owner (no assignee needed), advancing the graph.
	patchPath := "/v1/workgraphs/" + strconv.FormatInt(id, 10) + "/nodes/work"
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPatch, patchPath,
		map[string]any{"status": "done"}), driver))
	require.Equal(t, http.StatusOK, rec.Code, "driver settles the ai node; body=%s", rec.Body.String())

	// Next tick: the daemon runs the now-ready done(tool) node mechanically and the
	// instance reaches its terminal state — driven to completion by the owner.
	agentd.RunWorkgraphEngineTickForTest()

	doneNode, _ := db.GetWorkgraphNode(id, "done")
	assert.Equal(t, "done", doneNode.Status, "the trailing tool node runs mechanically after the driver advances the graph")
	inst, _ = db.GetWorkgraphInstance(id)
	assert.Equal(t, "completed", inst.Status, "the agent-driven instance reaches terminal via driver judgment + daemon tool-run")
}

// Scenario: an engine:agent instance with NO driver action does NOT make progress
// on its ai node — the daemon leaves it ready indefinitely (it never auto-spawns
// in agent mode). Distinct from the headline test by asserting the negative across
// multiple ticks: the suppression is durable, not a one-tick timing artefact.
func TestWorkgraphEngine_AgentModeNeverAutoSpawnsAINode(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))

	g := f.HaveGroup("squad")

	root := t.TempDir()
	writeToolTemplate(t, root, "agentidle",
		"name: agentidle\nengine: agent\nentry: work\n",
		"flowchart TD\n work --> done\n",
		map[string]string{
			"work": "executor:\n  kind: ai\n  agent: worker\n  prompt: do the work\n",
			"done": "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	body := map[string]any{"template_ref": "project:agentidle", "group": "squad"}
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/workgraphs", body)))
	require.Equal(t, http.StatusOK, rec.Code, "create body=%s", rec.Body.String())
	var created struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	_ = g

	// Several ticks — the daemon never auto-spawns the entry ai node in agent mode.
	for range 5 {
		agentd.RunWorkgraphEngineTickForTest()
	}

	work, _ := db.GetWorkgraphNode(created.ID, "work")
	assert.Equal(t, "ready", work.Status, "the ai node stays ready across ticks — no daemon auto-spawn in agent mode")
	assert.Empty(t, work.Assignee, "still unassigned — the daemon never spawned a worker")
	inst, _ := db.GetWorkgraphInstance(created.ID)
	assert.Equal(t, "running", inst.Status, "the instance stalls awaiting its driver, rather than progressing on its own")
}

// Scenario: the daemon's runaway-loop guard (max_visits) STILL fires in agent
// mode. The agent supplies judgment, but the daemon still mechanically runs tool
// nodes AND enforces their guards — so a self-looping tool node halts at the cap
// exactly as in system mode (the gate only suppresses the ai-spawn/handoff passes,
// never the tool drain or its guards).
func TestWorkgraphEngine_AgentModeStillEnforcesToolMaxVisits(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))
	t.Cleanup(agentd.SetWorkgraphMaxVisitsForTest(3)) // small cap for a deterministic break

	root := t.TempDir()
	writeToolTemplate(t, root, "agentcap",
		"name: agentcap\nengine: agent\nentry: impl\n",
		"flowchart TD\n impl --> test\n test -->|fail| impl\n test -->|pass| done\n",
		map[string]string{
			"impl": "executor:\n  kind: tool\n  run: echo implementing\n",
			"test": "executor:\n  kind: tool\n  run: echo testing\nverify:\n  kind: tool\n  run: exit 1\non_fail: continue\n",
			"done": "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	// Unbound — a pure tool loop needs no group; this isolates the guard.
	id := v1Create(t, f, "project:agentcap", "")
	inst, _ := db.GetWorkgraphInstance(id)
	require.Equal(t, "agent", inst.EngineMode, "instance is agent-mode")

	for range 20 {
		agentd.RunWorkgraphEngineTickForTest()
	}

	inst, _ = db.GetWorkgraphInstance(id)
	assert.Equal(t, "failed", inst.Status, "max_visits halts the runaway loop even in agent mode")
	impl, _ := db.GetWorkgraphNode(id, "impl")
	assert.Equal(t, int64(3), impl.Visits, "impl ran exactly the cap (3) times, never more")
}

// Scenario: the /v1 spawn-into-node verb's authz (F2). The driver authority is
// group-OWNERship of the instance's bound group — the same graph-level drive
// authority that already settles any node — so a non-owner agent is refused (403)
// and the refused call performs no spawn, while the group-owner is allowed.
func TestWorkgraphV1_SpawnIntoNodeAuthz(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))

	g := f.HaveGroup("squad")
	const owner = "ownr-aaaa-bbbb-cccc-1010"
	const other = "othr-aaaa-bbbb-cccc-2020"
	f.HaveConvWithTitle(owner, "owner")
	f.HaveConvWithTitle(other, "bystander")
	require.NoError(t, db.AddAgentGroupOwner(g.ID, owner, "human"), "AddAgentGroupOwner")

	// An entry ai node is ready immediately — the spawn target.
	root := t.TempDir()
	writeToolTemplate(t, root, "spawnauthz",
		"name: spawnauthz\nengine: agent\nentry: work\n",
		"flowchart TD\n work --> done\n",
		map[string]string{
			"work": "executor:\n  kind: ai\n  agent: worker\n  prompt: do the work\n",
			"done": "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	id := v1Create(t, f, "project:spawnauthz", "squad")
	startPath := "/v1/workgraphs/" + strconv.FormatInt(id, 10) + "/nodes/work/start"

	// A non-owner agent is refused, and the refused call performs no spawn.
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, startPath, nil), other))
	assert.Equal(t, http.StatusForbidden, rec.Code, "a non-owner agent cannot spawn into a node; body=%s", rec.Body.String())
	work, _ := db.GetWorkgraphNode(id, "work")
	assert.Equal(t, "ready", work.Status, "the refused call did not spawn — the node is untouched")
	assert.Empty(t, work.Assignee)

	// The group-owner is allowed and the node goes running.
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, startPath, nil), owner))
	require.Equal(t, http.StatusOK, rec.Code, "the group-owner may spawn into a node; body=%s", rec.Body.String())
	work, _ = db.GetWorkgraphNode(id, "work")
	assert.Equal(t, "running", work.Status, "the owner's spawn moved the node to running")
	assert.NotEmpty(t, work.Assignee, "spawned worker recorded as assignee")
}

// Scenario (JOH-15 B2a — the --context seed channel): the driver's spawn-into-node
// folds its --context seed into the spawned worker's startup brief, ADDITIVE to the
// node's task prompt. This is how an agent-engine driver routes an upstream AI
// node's output to a downstream worker (the daemon does NOT auto-handoff in agent
// mode; the driver owns data routing — the B1 contract).
func TestWorkgraphV1_SpawnIntoNodeSeedsContext(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))

	g := f.HaveGroup("squad")
	const driver = "drvr-aaaa-bbbb-cccc-5555"
	f.HaveConvWithTitle(driver, "engine-driver")
	require.NoError(t, db.AddAgentGroupOwner(g.ID, driver, "human"), "AddAgentGroupOwner")

	root := t.TempDir()
	writeToolTemplate(t, root, "seedctx",
		"name: seedctx\nengine: agent\nentry: work\n",
		"flowchart TD\n work --> done\n",
		map[string]string{
			"work": "executor:\n  kind: ai\n  agent: worker\n  prompt: do the work\n",
			"done": "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	id := v1Create(t, f, "project:seedctx", "squad")
	startPath := "/v1/workgraphs/" + strconv.FormatInt(id, 10) + "/nodes/work/start"

	const seed = "Upstream investigate node found: the bug is in parser.go:42"
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, startPath, map[string]any{"context": seed}), driver))
	require.Equal(t, http.StatusOK, rec.Code, "driver spawns with --context; body=%s", rec.Body.String())
	var startResp struct {
		Assignee string `json:"assignee"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &startResp))
	require.NotEmpty(t, startResp.Assignee, "spawned worker conv recorded as assignee")

	// The spawned worker's startup brief (an inbox message) carries BOTH the node's
	// task prompt AND the driver's seed, under the driver-context delimiter.
	msgs, err := db.ListAgentMessagesForConv(startResp.Assignee, 10)
	require.NoError(t, err)
	var brief string
	for _, m := range msgs {
		if strings.Contains(m.Body, "do the work") {
			brief = m.Body
		}
	}
	require.NotEmpty(t, brief, "spawned worker received a startup brief carrying the node task")
	assert.Contains(t, brief, seed, "the --context seed is folded into the worker's brief")
	assert.Contains(t, brief, "Context from the workgraph driver:", "seed sits under the driver-context delimiter")
}

// Scenario (JOH-15 B2a — per-node max_visits holds on the driver spawn path): the
// per-node execution cap (JOH-39) is a substrate guarantee in BOTH engine modes, so
// the manual/driver spawn-into-node path must enforce it exactly as the engine's own
// claimNextAINode does — else an agent-engine driver could re-spawn a looped-back ai
// node past its cap. A node that has already used its full visit budget (looped back
// to ready) refuses a fresh driver spawn with a 409, and the refused spawn bumps
// nothing.
func TestWorkgraphV1_SpawnIntoNodeEnforcesMaxVisits(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))
	t.Cleanup(agentd.SetWorkgraphMaxVisitsForTest(2)) // small default cap

	g := f.HaveGroup("squad")
	const driver = "drvr-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(driver, "engine-driver")
	require.NoError(t, db.AddAgentGroupOwner(g.ID, driver, "human"), "AddAgentGroupOwner")

	root := t.TempDir()
	writeToolTemplate(t, root, "capspawn",
		"name: capspawn\nengine: agent\nentry: work\n",
		"flowchart TD\n work --> done\n",
		map[string]string{
			"work": "executor:\n  kind: ai\n  agent: worker\n  prompt: do the work\n",
			"done": "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	id := v1Create(t, f, "project:capspawn", "squad")
	startPath := "/v1/workgraphs/" + strconv.FormatInt(id, 10) + "/nodes/work/start"

	// Simulate a node that looped back to ready having already spent its full visit
	// budget — the engine bumps Visits on each spawn, and a back-edge re-readies it
	// WITHOUT resetting Visits. The cap check must now refuse a fresh driver spawn.
	atCap := int64(2)
	ready := db.WorkgraphNodeStatusReady
	_, err := db.UpdateWorkgraphNode(id, "work", db.WorkgraphNodePatch{Status: &ready, Visits: &atCap})
	require.NoError(t, err)

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, startPath, nil), driver))
	assert.Equal(t, http.StatusConflict, rec.Code, "over-cap driver spawn is refused; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "max_visits", "the refusal names the cap")

	// The refused spawn did no work: node still ready, unassigned, Visits not bumped
	// past the cap (the spawn path cannot exceed max_visits in agent mode).
	work, _ := db.GetWorkgraphNode(id, "work")
	require.NotNil(t, work)
	assert.Equal(t, "ready", work.Status, "refused spawn leaves the node ready")
	assert.Empty(t, work.Assignee, "no worker assigned by a refused spawn")
	assert.Equal(t, int64(2), work.Visits, "Visits not bumped past the cap by the refused spawn")
}

// Scenario (JOH-15 B2a — `workgraph drive` anchoring): anchoring a driver spawns a
// fresh agent into the instance's bound group, grants it group-OWNERSHIP (its F2
// drive authority), and briefs it to run the workgraph-engine skill against this
// instance. A second anchor warns (the one-driver-per-instance v1 contract).
func TestWorkgraphV1_DriveAnchorsOwnerDriver(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))

	g := f.HaveGroup("squad")

	root := t.TempDir()
	writeToolTemplate(t, root, "drivable",
		"name: drivable\nengine: agent\nentry: work\n",
		"flowchart TD\n work --> done\n",
		map[string]string{
			"work": "executor:\n  kind: ai\n  agent: worker\n  prompt: do the work\n",
			"done": "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	id := v1Create(t, f, "project:drivable", "squad")
	drivePath := "/v1/workgraphs/" + strconv.FormatInt(id, 10) + "/drive"

	// Human anchors the driver.
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, drivePath, nil)))
	require.Equal(t, http.StatusOK, rec.Code, "human anchors a driver; body=%s", rec.Body.String())
	var resp struct {
		DriverConv string `json:"driver_conv"`
		Group      string `json:"group"`
		Warning    string `json:"warning"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.DriverConv, "anchored driver conv-id returned")
	assert.Equal(t, "squad", resp.Group)
	assert.Empty(t, resp.Warning, "no live agent-owner before the first driver → no warning")

	// The driver is a group OWNER — that ownership is its drive authority (F2).
	owners, err := db.ListAgentGroupOwners(g.ID)
	require.NoError(t, err)
	var isOwner bool
	for _, o := range owners {
		if o.ConvID == resp.DriverConv {
			isOwner = true
		}
	}
	assert.True(t, isOwner, "the anchored driver %q is a group owner", resp.DriverConv)

	// Its kickoff brief points at the workgraph-engine skill and names the instance.
	msgs, err := db.ListAgentMessagesForConv(resp.DriverConv, 10)
	require.NoError(t, err)
	var brief string
	for _, m := range msgs {
		if strings.Contains(m.Body, "workgraph-engine") {
			brief = m.Body
		}
	}
	require.NotEmpty(t, brief, "driver briefed to run the workgraph-engine skill")
	assert.Contains(t, brief, "instance "+strconv.FormatInt(id, 10), "brief names the instance it drives")

	// A SECOND anchor warns: the first driver is now a live agent-owner of the group.
	rec2 := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, drivePath, nil)))
	require.Equal(t, http.StatusOK, rec2.Code, "second drive still succeeds (warn, not block); body=%s", rec2.Body.String())
	var resp2 struct {
		Warning string `json:"warning"`
	}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.NotEmpty(t, resp2.Warning, "a second anchor warns about the one-driver-per-instance contract")
}

// Scenario: `workgraph drive` refuses a system-mode instance — the deterministic
// engine already drives it, so a driver would only double-spawn. The driver verb is
// strictly for engine:agent instances.
func TestWorkgraphV1_DriveRefusesSystemMode(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))

	f.HaveGroup("squad")

	root := t.TempDir()
	// No `engine:` line → default system mode.
	writeToolTemplate(t, root, "sysmode",
		"name: sysmode\nentry: work\n",
		"flowchart TD\n work --> done\n",
		map[string]string{
			"work": "executor:\n  kind: ai\n  agent: worker\n  prompt: do the work\n",
			"done": "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	id := v1Create(t, f, "project:sysmode", "squad")
	inst, _ := db.GetWorkgraphInstance(id)
	require.Equal(t, "system", inst.EngineMode, "instance is system-mode")

	drivePath := "/v1/workgraphs/" + strconv.FormatInt(id, 10) + "/drive"
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, drivePath, nil)))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "drive refuses a system-mode instance; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "engine:agent", "the refusal explains drive is for engine:agent instances")
}

// Scenario (JOH-15 B2b — the event-driven driver wake): in agent mode the daemon
// nudges the live driver on a frontier change (taking the slot of the suppressed
// handoff pass), and does so IDEMPOTENTLY — once per distinct change, not every tick.
// A node sitting ready across ticks nudges once; a real advance (settle → a new node
// readies) produces exactly one more nudge.
func TestWorkgraphEngine_AgentModeNudgesDriverOnFrontierChange(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))

	f.HaveGroup("squad")

	root := t.TempDir()
	// Two ai nodes so settling the first READIES the second (instance stays running —
	// a clean, non-terminal frontier change) before the trailing tool node.
	writeToolTemplate(t, root, "nudgegraph",
		"name: nudgegraph\nengine: agent\nentry: work\n",
		"flowchart TD\n work --> review\n review --> done\n",
		map[string]string{
			"work":   "executor:\n  kind: ai\n  agent: worker\n  prompt: do the work\n",
			"review": "executor:\n  kind: ai\n  agent: reviewer\n  prompt: review it\n",
			"done":   "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	id := v1Create(t, f, "project:nudgegraph", "squad")
	idStr := strconv.FormatInt(id, 10)

	// Anchor a LIVE driver (spawns a live owner-agent into the bound group — the
	// resolveDriverTargets heuristic resolves to exactly this driver).
	driveRec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/workgraphs/"+idStr+"/drive", nil)))
	require.Equal(t, http.StatusOK, driveRec.Code, "anchor driver; body=%s", driveRec.Body.String())
	var drv struct {
		DriverConv string `json:"driver_conv"`
	}
	require.NoError(t, json.Unmarshal(driveRec.Body.Bytes(), &drv))
	require.NotEmpty(t, drv.DriverConv)

	countFrontierNudges := func() int {
		msgs, err := db.ListAgentMessagesForConv(drv.DriverConv, 50)
		require.NoError(t, err)
		n := 0
		for _, m := range msgs {
			if strings.Contains(m.Subject, "frontier changed") {
				n++
			}
		}
		return n
	}

	// Tick 1: the entry node `work` is ready → the driver is nudged once.
	agentd.RunWorkgraphEngineTickForTest()
	assert.Equal(t, 1, countFrontierNudges(), "driver nudged once when the entry node became ready")

	// Ticks 2–3: frontier unchanged → NO additional nudge (idempotent, not per-tick).
	agentd.RunWorkgraphEngineTickForTest()
	agentd.RunWorkgraphEngineTickForTest()
	assert.Equal(t, 1, countFrontierNudges(), "no re-nudge while the frontier is unchanged")

	// Driver advances the frontier: spawn a worker into `work` (→ running, not an
	// actionable state, so no nudge), then settle it done → `review` becomes ready.
	startRec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/workgraphs/"+idStr+"/nodes/work/start", nil), drv.DriverConv))
	require.Equal(t, http.StatusOK, startRec.Code, "driver spawns into work; body=%s", startRec.Body.String())
	patchRec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPatch,
		"/v1/workgraphs/"+idStr+"/nodes/work", map[string]any{"status": "done"}), drv.DriverConv))
	require.Equal(t, http.StatusOK, patchRec.Code, "driver settles work done; body=%s", patchRec.Body.String())

	// Tick: `review` is now ready (and `work` is done) — a NEW frontier → exactly one
	// more nudge (both changes batch into a single wake). Instance stays running.
	agentd.RunWorkgraphEngineTickForTest()
	assert.Equal(t, 2, countFrontierNudges(), "a real frontier change produces exactly one more nudge")
	inst, _ := db.GetWorkgraphInstance(id)
	assert.Equal(t, "running", inst.Status, "instance still running (review is ready, not terminal)")
}

// Scenario (JOH-15 B2b — the daemon-side rescue half of the hybrid wake): with NO
// live driver, the nudge pass emits nothing AND records no marker, so the frontier
// is NOT permanently "consumed". When a driver is later anchored (or returns from
// reincarnation), the still-current frontier reads as new and nudges it. This is the
// daemon-side complement to the skill's slow heartbeat — together they ensure a
// momentarily-absent driver never misses the frontier. (The driver's own slow
// heartbeat self-poll is skill-level and is covered by the manual verify recipe.)
func TestWorkgraphEngine_AgentModeFrontierSurvivesAbsentDriver(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))

	g := f.HaveGroup("squad")
	// An owner with no running session — present but NOT live, so it's not a nudge
	// target (the human owner, or a driver that has died, looks like this).
	const dead = "dead-aaaa-bbbb-cccc-0000"
	require.NoError(t, db.AddAgentGroupOwner(g.ID, dead, "human"), "AddAgentGroupOwner")

	root := t.TempDir()
	writeToolTemplate(t, root, "absentdrv",
		"name: absentdrv\nengine: agent\nentry: work\n",
		"flowchart TD\n work --> done\n",
		map[string]string{
			"work": "executor:\n  kind: ai\n  agent: worker\n  prompt: do the work\n",
			"done": "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	id := v1Create(t, f, "project:absentdrv", "squad")
	idStr := strconv.FormatInt(id, 10)

	// Ticks with no LIVE driver: no nudge fires, and crucially NO marker is burned —
	// so the frontier isn't silently consumed.
	for range 3 {
		agentd.RunWorkgraphEngineTickForTest()
	}
	events, err := db.ListWorkgraphEvents(id)
	require.NoError(t, err)
	for _, e := range events {
		assert.NotEqual(t, db.WorkgraphEventDriverNudge, e.Kind, "no driver-nudge marker written without a live driver")
	}

	// Anchor a live driver now. The still-ready entry frontier must nudge it — proof
	// the earlier absence didn't permanently consume the change.
	driveRec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/workgraphs/"+idStr+"/drive", nil)))
	require.Equal(t, http.StatusOK, driveRec.Code, "anchor driver; body=%s", driveRec.Body.String())
	var drv struct {
		DriverConv string `json:"driver_conv"`
	}
	require.NoError(t, json.Unmarshal(driveRec.Body.Bytes(), &drv))

	agentd.RunWorkgraphEngineTickForTest()
	msgs, err := db.ListAgentMessagesForConv(drv.DriverConv, 50)
	require.NoError(t, err)
	var nudged bool
	for _, m := range msgs {
		if strings.Contains(m.Subject, "frontier changed") {
			nudged = true
		}
	}
	assert.True(t, nudged, "a newly-anchored driver is nudged about the still-current frontier")
}

// Scenario (JOH-15 B2b — per-recipient nudge markers): the idempotency marker is
// keyed per recipient conv (like handoffMarker's @conv), so a driver that joins —
// or is handed over / reincarnated under a NEW conv — AFTER an earlier driver was
// already nudged still gets nudged for the same still-pending frontier, while the
// earlier one is NOT re-nudged. This is the daemon-side guarantee that a driver
// handover doesn't drop the frontier (the reincarnation-robustness the marker keying
// buys).
func TestWorkgraphEngine_AgentModeNudgesNewlyJoinedDriver(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))

	f.HaveGroup("squad")

	root := t.TempDir()
	writeToolTemplate(t, root, "joindrv",
		"name: joindrv\nengine: agent\nentry: work\n",
		"flowchart TD\n work --> done\n",
		map[string]string{
			"work": "executor:\n  kind: ai\n  agent: worker\n  prompt: do the work\n",
			"done": "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	id := v1Create(t, f, "project:joindrv", "squad")
	idStr := strconv.FormatInt(id, 10)
	drivePath := "/v1/workgraphs/" + idStr + "/drive"

	frontierNudges := func(conv string) int {
		msgs, err := db.ListAgentMessagesForConv(conv, 50)
		require.NoError(t, err)
		n := 0
		for _, m := range msgs {
			if strings.Contains(m.Subject, "frontier changed") {
				n++
			}
		}
		return n
	}
	anchor := func() string {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, drivePath, nil)))
		require.Equal(t, http.StatusOK, rec.Code, "anchor driver; body=%s", rec.Body.String())
		var r struct {
			DriverConv string `json:"driver_conv"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &r))
		require.NotEmpty(t, r.DriverConv)
		return r.DriverConv
	}

	// Driver A anchored; tick → A nudged about the ready entry node.
	driverA := anchor()
	agentd.RunWorkgraphEngineTickForTest()
	require.Equal(t, 1, frontierNudges(driverA), "A nudged about the ready entry node")

	// Driver B joins later (a handover/reincarnation stand-in: a NEW live owner conv).
	driverB := anchor()
	require.NotEqual(t, driverA, driverB)

	// Tick: the SAME entry node is still ready. Per-conv markers → B is nudged for it
	// (its @conv marker is fresh), while A is NOT re-nudged (already current).
	agentd.RunWorkgraphEngineTickForTest()
	assert.Equal(t, 1, frontierNudges(driverB), "the newly-joined driver is nudged about the still-pending frontier")
	assert.Equal(t, 1, frontierNudges(driverA), "the earlier driver is not re-nudged for the unchanged frontier")
}

// blockingSpawner wraps a base Spawner and stalls SpawnNew until released, letting a
// test hold a spawn "in flight" and observe what runs concurrently. SpawnResume
// passes straight through. The first SpawnNew closes `entered` (signalling the spawn
// is in flight) then blocks on `release`.
type blockingSpawner struct {
	base    agentd.Spawner
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingSpawner) SpawnNew(label, cwd, effort string) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.base.SpawnNew(label, cwd, effort)
}

func (b *blockingSpawner) SpawnResume(convID, cwd string) error {
	return b.base.SpawnResume(convID, cwd)
}

// Scenario (JOH-15 B2a — the off-lock spawn fix): a driver's spawn-into-node holds
// NO per-instance lock across the ~30s conv-id handshake, so a concurrent engine
// tick on that instance is never stalled behind it. We block the spawn mid-flight
// (after the claim released the lock, before the conv-id materialises) and prove an
// engine tick still completes promptly — the regression test for B1's lock-held
// carry-over. Run under -race: the claim/settle and the concurrent tick touch the
// same node + per-instance lock from different goroutines.
func TestWorkgraphEngine_StartSpawnRunsOffInstanceLock(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	t.Cleanup(agentd.SetWorkgraphEngineEnabledForTest(true))

	// Shadow the sim spawner with a blocking wrapper so SpawnNew stalls until we
	// release. newFlow installed agentd.Spawn = simSpawner and registered the cleanup
	// that restores the production original; this re-assignment rides that cleanup.
	blk := &blockingSpawner{base: agentd.Spawn, entered: make(chan struct{}), release: make(chan struct{})}
	agentd.Spawn = blk
	// Always release the blocked spawn, even on a failed assertion, so the background
	// /v1 goroutine can't leak past the test.
	var releaseOnce sync.Once
	releaseSpawn := func() { releaseOnce.Do(func() { close(blk.release) }) }
	t.Cleanup(releaseSpawn)

	g := f.HaveGroup("squad")
	const driver = "drvr-aaaa-bbbb-cccc-7777"
	f.HaveConvWithTitle(driver, "engine-driver")
	require.NoError(t, db.AddAgentGroupOwner(g.ID, driver, "human"), "AddAgentGroupOwner")

	root := t.TempDir()
	writeToolTemplate(t, root, "offlock",
		"name: offlock\nengine: agent\nentry: work\n",
		"flowchart TD\n work --> done\n",
		map[string]string{
			"work": "executor:\n  kind: ai\n  agent: worker\n  prompt: do the work\n",
			"done": "executor:\n  kind: tool\n  run: echo shipped\n",
		})
	t.Cleanup(agentd.SetWorkgraphProjectDirsForTest(root))

	id := v1Create(t, f, "project:offlock", "squad")

	// Build the request on the test goroutine (JSONRequest may t.Fatal), then fire
	// the /v1 spawn-into-node in the background. It claims the node under the lock,
	// RELEASES the lock, then blocks inside executeSpawn (our blockingSpawner) — the
	// spawn is now in flight with no lock held.
	startPath := "/v1/workgraphs/" + strconv.FormatInt(id, 10) + "/nodes/work/start"
	startReq := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, startPath, nil), driver)
	startDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { startDone <- testharness.Serve(f.Mux, startReq) }()

	// Wait until the spawn is actually in flight (claim done, lock released).
	select {
	case <-blk.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("spawn never reached executeSpawn — the claim phase stalled")
	}

	// Mid-flight the node is claimed running (the off-lock claim landed and freed the
	// lock before the spawn began blocking).
	work, _ := db.GetWorkgraphNode(id, "work")
	require.NotNil(t, work)
	assert.Equal(t, "running", work.Status, "node claimed running while the spawn is in flight")

	// THE ASSERTION: with the spawn in flight, a concurrent engine tick must NOT
	// stall. The tick takes the SAME per-instance lock every pass; under B1's
	// lock-held spawn it would block until release. Off-lock, it returns at once.
	tickDone := make(chan struct{})
	go func() {
		agentd.RunWorkgraphEngineTickForTest()
		close(tickDone)
	}()
	select {
	case <-tickDone:
		// PASS — the tick ran while the spawn held no lock.
	case <-time.After(5 * time.Second):
		t.Fatal("engine tick stalled while a /v1 start's spawn was in flight — the " +
			"per-instance lock is held across executeSpawn (off-lock fix regressed)")
	}

	// Release the spawn; the /v1 call finishes and the node settles to the real
	// conv-id (sentinel swapped) with its visit counted.
	releaseSpawn()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-startDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the /v1 start never returned after the spawn was released")
	}
	require.Equal(t, http.StatusOK, rec.Code, "start completes after release; body=%s", rec.Body.String())
	var startResp struct {
		Status   string `json:"status"`
		Assignee string `json:"assignee"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &startResp))
	require.NotEmpty(t, startResp.Assignee, "spawned worker conv recorded as assignee")

	work, _ = db.GetWorkgraphNode(id, "work")
	assert.Equal(t, "running", work.Status, "node still running, now driven by the live worker")
	assert.Equal(t, startResp.Assignee, work.Assignee, "the engine sentinel was swapped for the real conv-id on settle")
	assert.Equal(t, int64(1), work.Visits, "the spawn counted one execution (settleAISpawn bumps Visits)")
}
