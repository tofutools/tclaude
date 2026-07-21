package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// effectiveView mirrors the daemon's targeted GET /v1/permissions
// response — the resolved identity + effective-permission view the CLI
// renders (TCL-611).
type effectiveView struct {
	Target       string   `json:"target"`
	TargetKey    string   `json:"target_key"`
	AgentID      string   `json:"agent_id"`
	Title        string   `json:"title"`
	Effective    []string `json:"effective"`
	Source       string   `json:"source"`
	OwnerImplied []string `json:"owner_implied"`
}

// getPermissionsTarget performs the targeted read as an agent peer — the
// caller shape that matters here, since the whole point is that an agent
// (which cannot read ~/.tclaude/data) gets a complete answer from the
// daemon.
func getPermissionsTarget(t *testing.T, f *testharness.Flow, callerConv, target string) *httpResult {
	t.Helper()
	path := "/v1/permissions?target=" + url.QueryEscape(target)
	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, path, nil), callerConv))
	return &httpResult{Code: rec.Code, Body: rec.Body.String()}
}

func decodeEffective(t *testing.T, r *httpResult) effectiveView {
	t.Helper()
	var v effectiveView
	require.NoError(t, json.Unmarshal([]byte(r.Body), &v), "decode effective view: %s", r.Body)
	return v
}

// Scenario: a sandboxed agent asks the daemon for another agent's
// effective permissions by TITLE, by full conv-id and by stable agent-id
// prefix. Every form must resolve daemon-side and come back with the
// identity + display metadata the CLI needs, so the client never has to
// open the private DB itself.
func TestPermissionsLsTarget_ResolvesEverySelectorForm(t *testing.T) {
	f := newFlow(t)

	const caller = "pltc-aaaa-bbbb-cccc-0001"
	const target = "pltt-aaaa-bbbb-cccc-0002"
	f.HaveConvWithTitle(caller, "sandboxed-agent")
	f.HaveConvWithTitle(target, "sandbox-lead")
	f.HaveEnrolledAgent(target)

	agentID, err := db.AgentIDForConv(target)
	require.NoError(t, err, "AgentIDForConv")
	require.NotEmpty(t, agentID, "target must be enrolled as an agent")

	permMutate(t, f, "grant", "default", "self.rename")
	permMutate(t, f, "grant", target, "permissions.grant")

	for _, tc := range []struct {
		name     string
		selector string
	}{
		{"by title", "sandbox-lead"},
		{"by conv-id", target},
		{"by agent-id", agentID},
		{"by agent-id prefix", agentID[:12]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := getPermissionsTarget(t, f, caller, tc.selector)
			require.Equalf(t, http.StatusOK, res.Code, "GET body=%s", res.Body)
			v := decodeEffective(t, res)
			assert.Equal(t, tc.selector, v.Target, "target echoes the selector as typed")
			assert.Equal(t, target, v.TargetKey, "resolved conv-id")
			assert.Equal(t, agentID, v.AgentID, "stable agent_id comes back for display")
			assert.Equal(t, "sandbox-lead", v.Title, "display title comes back for rendering")
			assert.Contains(t, v.Effective, "self.rename", "global default is in the effective set")
			assert.Contains(t, v.Effective, "permissions.grant", "per-conv grant is in the effective set")
			assert.Contains(t, v.Source, "defaults+grants:", "source names the matched inputs")
		})
	}
}

// Scenario: the reported bug. A stale title (the agent has since been
// renamed) must come back as ONE concise typed not_found — no raw
// ~/.tclaude/data path, no internal filesystem error text.
func TestPermissionsLsTarget_StaleSelectorIsConciseNotFound(t *testing.T) {
	f := newFlow(t)

	const caller = "plst-aaaa-bbbb-cccc-0001"
	const target = "plst-aaaa-bbbb-cccc-0002"
	f.HaveConvWithTitle(caller, "sandboxed-agent")
	f.HaveConvWithTitle(target, "sandbox-lead") // renamed away from "sandbox-test-codex"

	res := getPermissionsTarget(t, f, caller, "sandbox-test-codex")
	require.Equal(t, http.StatusNotFound, res.Code, "stale selector body=%s", res.Body)

	var envelope struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Body), &envelope), "decode error envelope")
	assert.Equal(t, "not_found", envelope.Code, "typed not_found")
	assert.Contains(t, envelope.Error, "sandbox-test-codex", "names the selector that missed")
	assertNoPrivatePaths(t, envelope.Error)
}

// Scenario: two conversations share a display title. The daemon answers
// with a typed ambiguity plus usable candidates, so the CLI can list them
// without resolving anything locally.
func TestPermissionsLsTarget_AmbiguousSelectorReturnsCandidates(t *testing.T) {
	f := newFlow(t)

	const caller = "plam-aaaa-bbbb-cccc-0001"
	const twinA = "plam-aaaa-bbbb-cccc-0002"
	const twinB = "plam-aaaa-bbbb-cccc-0003"
	f.HaveConvWithTitle(caller, "sandboxed-agent")
	f.HaveConvWithTitle(twinA, "twin")
	f.HaveConvWithTitle(twinB, "twin")

	res := getPermissionsTarget(t, f, caller, "twin")
	require.Equal(t, http.StatusConflict, res.Code, "ambiguous body=%s", res.Body)

	var envelope struct {
		Error      string `json:"error"`
		Code       string `json:"code"`
		Candidates []struct {
			ConvID string `json:"conv_id"`
			Title  string `json:"title"`
		} `json:"candidates"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Body), &envelope), "decode ambiguity envelope")
	assert.Equal(t, "ambiguous", envelope.Code, "typed ambiguity")
	require.Len(t, envelope.Candidates, 2, "both twins offered as candidates")
	convs := []string{envelope.Candidates[0].ConvID, envelope.Candidates[1].ConvID}
	assert.ElementsMatch(t, []string{twinA, twinB}, convs, "candidates are usable selectors")
	assertNoPrivatePaths(t, envelope.Error)
}

// Scenario: the target owns a group. Ownership structurally confers the
// owner-implied slugs, and the daemon must both fold them into the
// effective set and report which ones came SOLELY from ownership so the
// CLI can annotate them "(via ownership)". A deny override still wins.
func TestPermissionsLsTarget_OwnerImpliedAndDeny(t *testing.T) {
	f := newFlow(t)

	const caller = "ploi-aaaa-bbbb-cccc-0001"
	const owner = "ploi-aaaa-bbbb-cccc-0002"
	f.HaveConvWithTitle(caller, "sandboxed-agent")
	f.HaveConvWithTitle(owner, "squad-lead")

	g := f.HaveGroup("squad")
	require.NoError(t, db.AddAgentGroupOwner(g.ID, owner, "test"), "seed owner")

	v := decodeEffective(t, getPermissionsTarget(t, f, caller, owner))
	assert.Contains(t, v.Effective, agentd.PermGroupsSpawn, "owner holds the owner-implied slug")
	assert.Contains(t, v.OwnerImplied, agentd.PermGroupsSpawn, "and it is flagged as owner-conferred")
	assert.Contains(t, v.Source, "+owner", "source notes the ownership contribution")

	// A per-conv deny suppresses the owner bypass in both projections.
	permMutate(t, f, "deny", owner, agentd.PermGroupsSpawn)
	v = decodeEffective(t, getPermissionsTarget(t, f, caller, owner))
	assert.NotContains(t, v.Effective, agentd.PermGroupsSpawn, "deny beats the owner bypass")
	assert.NotContains(t, v.OwnerImplied, agentd.PermGroupsSpawn, "and the annotation goes with it")
	assert.Contains(t, v.Source, "−denies", "source notes the deny")
}

// Scenario: `permissions ls default`. The magic sentinel is answered
// daemon-side too, so the CLI needs no special case that reads config or
// the DB itself.
func TestPermissionsLsTarget_DefaultSentinel(t *testing.T) {
	f := newFlow(t)

	const caller = "plds-aaaa-bbbb-cccc-0001"
	f.HaveConvWithTitle(caller, "sandboxed-agent")
	permMutate(t, f, "grant", "default", "self.rename")

	v := decodeEffective(t, getPermissionsTarget(t, f, caller, "default"))
	assert.Equal(t, "default", v.Target, "sentinel echoes back")
	assert.Empty(t, v.TargetKey, "the sentinel is not a conv")
	assert.Contains(t, v.Effective, "self.rename", "defaults list comes back")
}

// Scenario: UNTARGETED listing. The roster response must carry the
// display metadata (agent_ids + titles) for every override key, so the
// CLI renders names without a client-side conv_index read.
func TestPermissionsLs_UntargetedCarriesDisplayMetadata(t *testing.T) {
	f := newFlow(t)

	const caller = "plum-aaaa-bbbb-cccc-0001"
	const target = "plum-aaaa-bbbb-cccc-0002"
	f.HaveConvWithTitle(caller, "sandboxed-agent")
	f.HaveConvWithTitle(target, "sandbox-lead")
	f.HaveEnrolledAgent(target)
	permMutate(t, f, "grant", target, "permissions.grant")

	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/permissions", nil), caller))
	require.Equal(t, http.StatusOK, rec.Code, "GET /v1/permissions body=%s", rec.Body.String())

	var state struct {
		Overrides map[string]map[string]string `json:"overrides"`
		AgentIDs  map[string]string            `json:"agent_ids"`
		Titles    map[string]string            `json:"titles"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &state), "decode state")
	require.Contains(t, state.Overrides, target, "override row present")
	assert.Equal(t, "sandbox-lead", state.Titles[target], "title projected for the CLI roster")
	assert.NotEmpty(t, state.AgentIDs[target], "stable agent_id projected for the CLI roster")
}

// Scenario: end-to-end through the CLI. `tclaude agent permissions ls
// <target>` runs against the real daemon mux and renders the resolved
// identity, the effective slugs and the ownership annotation — all from
// the daemon response. The stale-selector case prints exactly one
// concise error line with no private path in it.
func TestPermissionsLsCLI_RendersFromDaemonResponse(t *testing.T) {
	f := newFlow(t)

	const caller = "plcl-aaaa-bbbb-cccc-0001"
	const owner = "plcl-aaaa-bbbb-cccc-0002"
	f.HaveConvWithTitle(caller, "sandboxed-agent")
	f.HaveConvWithTitle(owner, "squad-lead")
	f.HaveEnrolledAgent(owner)
	g := f.HaveGroup("squad")
	require.NoError(t, db.AddAgentGroupOwner(g.ID, owner, "test"), "seed owner")
	permMutate(t, f, "grant", "default", "self.rename")

	bridgeAgentClientToMuxAsAgent(t, f.Mux, caller)

	t.Run("resolved target renders identity and ownership", func(t *testing.T) {
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		rc := agent.RunPermissionsLs("squad-lead", false, stdout, stderr)
		require.Equal(t, 0, rc, "rc, stderr=%s", stderr.String())
		out := stdout.String()
		assert.Contains(t, out, "squad-lead", "renders the resolved title")
		assert.Contains(t, out, "self.rename", "renders the effective slugs")
		assert.Contains(t, out, agentd.PermGroupsSpawn+"  (via ownership)",
			"annotates owner-conferred slugs; got:\n%s", out)
		assert.Empty(t, stderr.String(), "clean run writes nothing to stderr")
	})

	t.Run("stale selector prints one concise error", func(t *testing.T) {
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		rc := agent.RunPermissionsLs("sandbox-test-codex", false, stdout, stderr)
		assert.Equal(t, 1, rc, "stale selector exits not-found")
		assert.Empty(t, stdout.String(), "nothing rendered on a miss")
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		assert.Len(t, lines, 1, "exactly one error line; got:\n%s", stderr.String())
		assertNoPrivatePaths(t, stderr.String())
	})

	t.Run("untargeted roster renders daemon-supplied titles", func(t *testing.T) {
		permMutate(t, f, "grant", owner, "permissions.grant")
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		rc := agent.RunPermissionsLs("", false, stdout, stderr)
		require.Equal(t, 0, rc, "rc, stderr=%s", stderr.String())
		assert.Contains(t, stdout.String(), "squad-lead", "roster shows the daemon-supplied title")
	})
}

// assertNoPrivatePaths fails when agent-facing text leaks the private
// data directory or a raw filesystem error — the exact shape TCL-611
// reported ("stat /home/.../.tclaude/data/db.sqlite: permission denied").
func assertNoPrivatePaths(t *testing.T, text string) {
	t.Helper()
	for _, leak := range []string{".tclaude/data", "db.sqlite", "permission denied", "stat "} {
		assert.NotContainsf(t, text, leak,
			"agent-facing text must not leak %q; got: %s", leak, text)
	}
}
