package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A restricted peer only ever adds confinement to an agent: off to
// resource-only, either to an implementation that confines access. It never
// loosens one, switches between two that confine, or pins a harness mode.
func TestPeerAgentSandboxImplOnlyAddsConfinement(t *testing.T) {
	h := newPeerProcessHarness(t)
	spawn := h.f.AsHuman().Spawn("crew", "boxed")
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	h.f.AsHuman().Stop(spawn.ConvID, false)
	local := func(impl string) {
		t.Helper()
		rec := testharness.Serve(h.f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
			"/v1/agent/"+spawn.ConvID+"/sandbox-impl", map[string]any{"implementation": impl})))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	local(string(sandboxpolicy.ImplementationOff))
	path := "/api/agents/" + agentIDOf(t, spawn.ConvID) + "/sandbox-impl"
	set := func(body map[string]any) (int, string) { return h.call("POST", path, body) }

	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	code, body := h.call("GET", path, nil)
	require.Equal(t, 403, code, body)
	h.grant(t, agentd.PermAgentSandboxImplementation, "group=crew")
	code, body = h.call("GET", path, nil)
	require.Equal(t, 200, code, body)
	code, body = set(map[string]any{"implementation": "harness-builtin"})
	require.Equal(t, 403, code, "a spawn's sandbox choice takes its grant: %s", body)
	h.grant(t, agentd.PermGroupsMembersSpawnSandbox, "group=crew")
	code, body = set(map[string]any{"implementation": "harness-builtin", "sandbox": "danger-full-access"})
	require.Equal(t, 403, code, "no harness mode: %s", body)
	code, body = set(map[string]any{"implementation": "harness-builtin", "restart": true})
	require.Equal(t, 403, code, "a restart takes the lifecycle grants: %s", body)

	// (off to resource-only needs a host cgroup; the move itself is pinned
	// in TestPeerSandboxImplMoves.)
	code, body = set(map[string]any{"implementation": "harness-builtin"})
	require.Equal(t, 200, code, "to an implementation that confines access: %s", body)
	for _, loosen := range []string{"off", "resource-only", "tclaude-layer"} {
		code, body = set(map[string]any{"implementation": loosen})
		require.Equal(t, 403, code, "%s: %s", loosen, body)
	}
	code, body = set(map[string]any{"implementation": "harness-builtin"})
	require.Equal(t, 200, code, "the same implementation: %s", body)

	h.trustUnrestricted(t)
	code, body = set(map[string]any{"implementation": "off"})
	require.Equal(t, 200, code, "unrestricted trust assigns as the operator: %s", body)
}

// flattenStoredSandboxProfile resolves a stored profile with its includes, as
// a launch does.
func flattenStoredSandboxProfile(t *testing.T, name string) (sandboxpolicy.Profile, error) {
	t.Helper()
	all, err := db.ListSandboxProfiles()
	require.NoError(t, err)
	registry := map[string]*db.SandboxProfile{}
	for _, p := range all {
		registry[p.Name] = p
	}
	toPolicy := func(p *db.SandboxProfile) sandboxpolicy.Profile {
		return sandboxpolicy.Profile{Name: p.Name, Filesystem: p.Filesystem, NetworkAccess: p.NetworkAccess, Includes: p.Includes, Environment: p.Environment}
	}
	root := registry[name]
	require.NotNil(t, root, name)
	return sandboxpolicy.Flatten(toPolicy(root), func(n string) (*sandboxpolicy.Profile, error) {
		if p := registry[n]; p != nil {
			policy := toPolicy(p)
			return &policy, nil
		}
		return nil, nil
	})
}

// A restricted peer's sandbox profiles are its own and only narrow one
// include: they follow your operator's edits of it, never exceed it, and fail
// closed without it.
func TestPeerSandboxProfilesOnlyNarrowTheirInclude(t *testing.T) {
	h := newPeerProcessHarness(t)
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "base", NetworkAccess: sandboxpolicy.NetworkAccessInternet})
	require.NoError(t, err)
	create := func(body map[string]any) (int, string) { return h.call("POST", "/api/sandbox-profiles", body) }

	code, body := create(map[string]any{"name": "mine", "includes": []string{"base"}})
	require.Equal(t, 403, code, body)
	h.grant(t, agentd.PermSandboxProfilesManage, "")
	denied := t.TempDir()
	for _, refused := range []map[string]any{
		{"name": "x", "includes": []string{}},
		{"name": "x", "includes": []string{"base", "base"}},
		{"name": "x", "includes": []string{"base"}, "filesystem": []map[string]any{{"path": denied, "access": "write"}}},
		{"name": "x", "includes": []string{"base"}, "network_access": "internet"},
		{"name": "x", "includes": []string{"base"}, "pre_launch": []any{}},
		{"name": "x", "includes": []string{"base"}, "Filesystem_Root": "host"},
	} {
		code, body = create(refused)
		require.Equal(t, 403, code, "%v: %s", refused, body)
	}
	code, body = create(map[string]any{"name": "x", "includes": []string{"nowhere"}})
	require.Equal(t, 400, code, body)

	code, body = create(map[string]any{"name": "mine", "includes": []string{"base"}, "network_access": "none",
		"filesystem": []map[string]any{{"path": denied, "access": "deny"}}})
	require.Equal(t, 201, code, body)
	flat, err := flattenStoredSandboxProfile(t, "mine")
	require.NoError(t, err)
	require.Equal(t, sandboxpolicy.NetworkAccessNone, flat.NetworkAccess, "narrower than its include")

	// It follows your operator's edit of the include, and still narrows it.
	base, err := db.GetSandboxProfile("base")
	require.NoError(t, err)
	base.Environment = []db.SandboxEnvironmentEntry{{Name: "FROM_BASE", Value: "1"}}
	require.NoError(t, db.UpdateSandboxProfile(base))
	flat, err = flattenStoredSandboxProfile(t, "mine")
	require.NoError(t, err)
	require.Len(t, flat.Environment, 1, "follows the include")
	require.Equal(t, sandboxpolicy.NetworkAccessNone, flat.NetworkAccess)

	// Edits keep the include; your operator's profiles are not the peer's.
	code, body = h.call("PATCH", "/api/sandbox-profiles/mine", map[string]any{"name": "mine", "includes": []string{"other"}})
	require.Equal(t, 403, code, body)
	code, body = h.call("PATCH", "/api/sandbox-profiles/mine", map[string]any{"name": "mine2", "includes": []string{"base"}, "network_access": "none"})
	require.Equal(t, 200, code, "a rename keeps it the peer's: %s", body)
	code, body = h.call("GET", "/api/sandbox-profiles/mine2", nil)
	require.Equal(t, 200, code, body)
	code, body = h.call("PATCH", "/api/sandbox-profiles/mine2", map[string]any{"name": "mine2", "network_access": "none"})
	require.Equal(t, 200, code, "an edit that leaves the include out: %s", body)
	kept, err := db.GetSandboxProfile("mine2")
	require.NoError(t, err)
	require.Equal(t, []string{"base"}, kept.Includes, "keeps it")
	for _, method := range []string{"GET", "PATCH", "DELETE"} {
		code, body = h.call(method, "/api/sandbox-profiles/base", map[string]any{"name": "base"})
		require.Equal(t, 403, code, "%s your operator's profile: %s", method, body)
	}

	// Its include cannot vanish under it: deleting it is refused, and a
	// rename moves the reference.
	_, err = db.DeleteSandboxProfile("base")
	require.ErrorIs(t, err, db.ErrSandboxProfileIncludedBy)
	base, err = db.GetSandboxProfile("base")
	require.NoError(t, err)
	base.Name = "base-renamed"
	require.NoError(t, db.UpdateSandboxProfile(base))
	mine, err := db.GetSandboxProfile("mine2")
	require.NoError(t, err)
	require.Equal(t, []string{"base-renamed"}, mine.Includes)
	flat, err = flattenStoredSandboxProfile(t, "mine2")
	require.NoError(t, err)
	require.Len(t, flat.Environment, 1)

	// Assigned to a group, deleting it would leave the group with none.
	_, err = db.SetAgentGroupSandboxProfile("crew", "mine2")
	require.NoError(t, err)
	code, body = h.call("DELETE", "/api/sandbox-profiles/mine2", nil)
	require.Equal(t, 409, code, body)
	_, err = db.SetAgentGroupSandboxProfile("crew", "")
	require.NoError(t, err)

	// An agent spawned under it resumes by its name once its ID is gone, so a
	// delete and a recreate would move the agent onto whatever the new one
	// includes.
	agentID := havePerformerAgent(t, h.f, "crew", "boxed-conv", "boxed", "dev", "offline")
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &sandboxpolicy.Snapshot{
		Version: sandboxpolicy.SnapshotVersion,
		Applied: []sandboxpolicy.AppliedProfile{{Scope: sandboxpolicy.ScopeExplicit, ID: mine.ID + 1000, Name: "mine2"}},
	}))
	code, body = h.call("DELETE", "/api/sandbox-profiles/mine2", nil)
	require.Equal(t, 409, code, "an agent spawned under it: %s", body)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &sandboxpolicy.Snapshot{Version: sandboxpolicy.SnapshotVersion}))

	// Once your operator adds to it, it is theirs to change.
	mine, err = db.GetSandboxProfile("mine2")
	require.NoError(t, err)
	mine.HarnessConfig = sandboxpolicy.HarnessConfigAccessRead
	require.NoError(t, db.UpdateSandboxProfile(mine))
	for _, method := range []string{"PATCH", "DELETE"} {
		code, body = h.call(method, "/api/sandbox-profiles/mine2", map[string]any{"name": "mine2"})
		require.Equal(t, 403, code, "%s: %s", method, body)
	}
	mine.HarnessConfig = ""
	require.NoError(t, db.UpdateSandboxProfile(mine))
	code, body = h.call("DELETE", "/api/sandbox-profiles/mine2", nil)
	require.Less(t, code, 300, body)
}

// A missing include fails a peer profile's resolution rather than leaving it
// wider.
func TestPeerSandboxProfileWithoutItsIncludeFailsClosed(t *testing.T) {
	_, err := sandboxpolicy.Flatten(sandboxpolicy.Profile{Name: "mine", Includes: []string{"gone"}, NetworkAccess: sandboxpolicy.NetworkAccessNone},
		func(string) (*sandboxpolicy.Profile, error) { return nil, nil })
	require.Error(t, err)
}

// A config bundle a restricted peer stages carries only the sections it
// reads otherwise, never your node's config or default permissions.
func TestPeerConfigExportCarriesOnlyReadableSections(t *testing.T) {
	h := newPeerProcessHarness(t)
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "base"})
	require.NoError(t, err)
	export := "/api/config-bundle/export"
	code, body := h.stage(t, export)
	require.Equal(t, 403, code, body)
	h.grant(t, agentd.PermConfigExport, "")
	for _, refused := range []string{export + "?allow_flagged=true", export + "?only=config", export + "?only=profiles"} {
		code, body = h.stage(t, refused)
		require.Equal(t, 403, code, "%s: %s", refused, body)
	}
	read := func(path string) map[string][]json.RawMessage {
		t.Helper()
		code, body := h.stage(t, path)
		require.Equal(t, 200, code, body)
		code, body = h.call("GET", "/api/peer-downloads/"+url.PathEscape(stagedID(t, body))+"?offset=0&length=4194304", nil)
		require.Equal(t, 200, code, body)
		var chunk struct {
			Data []byte `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &chunk), body)
		var bundle struct {
			Sections map[string][]json.RawMessage `json:"sections"`
		}
		require.NoError(t, json.Unmarshal(chunk.Data, &bundle), string(chunk.Data))
		require.Contains(t, bundle.Sections, "default-permissions", "a real bundle")
		return bundle.Sections
	}
	sections := read(export)
	require.Empty(t, sections["sandbox-profiles"], "without node.spawn.read")
	require.Empty(t, sections["default-permissions"])
	require.Empty(t, sections["config"])
	h.grant(t, agentd.PermNodeSpawnRead, "")
	sections = read(export)
	require.NotEmpty(t, sections["sandbox-profiles"])
	require.Empty(t, sections["default-permissions"])
	require.Empty(t, sections["config"])

	// Revoking a section's grant stops a download that carries it.
	code, body = h.stage(t, export)
	require.Equal(t, 200, code, body)
	id := stagedID(t, body)
	h.revoke(t, agentd.PermNodeSpawnRead, "")
	code, body = h.call("GET", "/api/peer-downloads/"+url.PathEscape(id)+"?offset=0", nil)
	require.Equal(t, 403, code, body)
}
