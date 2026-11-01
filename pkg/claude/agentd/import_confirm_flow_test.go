package agentd_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/groupexport"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

const (
	confirmConv   = "ca11ab1e-0000-4000-8000-000000000001"
	confirmSecret = "hunter2-value"
)

type confirmView struct {
	Digest  string `json:"digest"`
	CanKeep bool   `json:"can_keep"`
	Items   []struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		Default bool   `json:"default"`
	} `json:"items"`
	Warnings []string `json:"warnings"`
}

func (c confirmView) ids() []string {
	out := []string{}
	for _, item := range c.Items {
		out = append(out, item.ID)
	}
	return out
}

// confirmGroupArchive holds one of everything a group file can ask for.
func confirmGroupArchive(t *testing.T, name string) []byte {
	t.Helper()
	exp := &groupexport.Export{
		FormatVersion: groupexport.FormatVersion,
		SourceGroup:   name,
		Group: groupexport.Group{
			Permissions: []groupexport.GroupPermission{{Slug: agentd.PermGroupsMembersSpawn, GrantedBy: "file"}},
			Environment: []sandboxpolicy.EnvironmentEntry{{Name: "TOKENISH", Value: confirmSecret}},
		},
		Members: []groupexport.Member{{ConvID: confirmConv, Role: "lead"}},
		Convs:   []groupexport.Conv{{ConvID: confirmConv, Title: "lead", Missing: true}},
		Owners:  []groupexport.Owner{{ConvID: confirmConv, GrantedBy: "file"}},
		Permissions: []groupexport.Permission{
			{ConvID: confirmConv, Slug: "groups.export", Effect: db.PermEffectGrant, GrantedBy: "file"},
			{ConvID: confirmConv, Slug: "self.rename", Effect: db.PermEffectDeny, GrantedBy: "file"},
		},
		SudoGrants: []groupexport.SudoGrant{{ConvID: confirmConv, Slug: "human.notify", GrantedBy: "file", ExpiresAt: "2099-01-01T00:00:00Z"}},
		CronJobs: []groupexport.CronJob{{ID: 1, Name: "nightly", OwnerConv: confirmConv, TargetConv: confirmConv, TargetKind: db.CronTargetConv,
			IntervalSeconds: 60, Body: "wake", Enabled: 1, OperatorAuthored: true, CreatedAt: "2026-01-01T00:00:00Z"}},
	}
	archive, err := groupexport.Marshal(exp)
	require.NoError(t, err)
	return archive
}

func inspectConfirm(t *testing.T, f *testharness.Flow, archive []byte, as func(*http.Request) *http.Request) (confirmView, []string, string) {
	t.Helper()
	rec := testharness.Serve(f.Mux, as(httptest.NewRequest(http.MethodPost, "/v1/groups/import/inspect", bytes.NewReader(archive))))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Confirm      confirmView `json:"confirm"`
		FileWarnings []string    `json:"file_warnings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.Confirm, out.FileWarnings, rec.Body.String()
}

func TestImportConfirm_PreviewListsTheFileWithoutValuesOrSudo(t *testing.T) {
	f := newFlow(t)
	confirm, warnings, body := inspectConfirm(t, f, confirmGroupArchive(t, "listed"), agentd.AsHumanPeer)

	assert.True(t, confirm.CanKeep)
	assert.ElementsMatch(t, []string{
		"owner:" + confirmConv, "group-permission:" + agentd.PermGroupsMembersSpawn,
		"permission:" + confirmConv + ":groups.export", "env:TOKENISH", "cron:nightly",
	}, confirm.ids(), "a deny only tightens, and sudo and the sandbox are never items")
	for _, item := range confirm.Items {
		assert.Equal(t, item.Kind != "environment" && item.Kind != "cron", item.Default, item.ID)
	}
	assert.NotContains(t, body, confirmSecret, "a preview never carries a value")
	assert.Contains(t, warnings[0]+warnings[1]+warnings[2], "Sudo is temporary")
	for _, w := range confirm.Warnings {
		assert.NotContains(t, w, "TOKENISH", "the list already shows it")
	}
}

// A restored group keeps exactly what was ticked, and the file's other items drop.
func TestImportConfirm_GroupKeepsWhatWasTicked(t *testing.T) {
	f := newFlow(t)
	archive := confirmGroupArchive(t, "restored")
	confirm, _, _ := inspectConfirm(t, f, archive, agentd.AsHumanPeer)

	q := url.Values{"into": {testutil.CanonicalTempDir(t)}, "keep_digest": {confirm.Digest},
		"keep": {"group-permission:" + agentd.PermGroupsMembersSpawn, "env:TOKENISH", "cron:nightly"}}
	rec, _ := importGroupWith(t, f, archive, q)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Kept, Dropped, FileWarnings []string
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &struct {
		Kept         *[]string `json:"kept"`
		Dropped      *[]string `json:"dropped"`
		FileWarnings *[]string `json:"file_warnings"`
	}{&out.Kept, &out.Dropped, &out.FileWarnings}))
	assert.Len(t, out.Kept, 3)
	assert.Len(t, out.Dropped, 2)
	assert.Contains(t, rec.Body.String(), "as group owners", "an unticked owner warns")

	g, err := db.GetAgentGroupByName("restored")
	require.NoError(t, err)
	owners, err := db.ListAgentGroupOwners(g.ID)
	require.NoError(t, err)
	assert.Empty(t, owners, "the unticked owner lands with no ownership")
	permissions, err := db.ListAgentGroupPermissions(g.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{agentd.PermGroupsMembersSpawn}, permissions)
	assert.Equal(t, []sandboxpolicy.EnvironmentEntry{{Name: "TOKENISH", Value: confirmSecret}}, g.Environment)
	overrides, err := db.ListAgentPermissionOverridesForConv(confirmConv)
	require.NoError(t, err)
	assert.Equal(t, db.PermEffectDeny, overrides["self.rename"], "a deny stays")
	assert.NotContains(t, overrides, "groups.export", "an unticked grant drops")
	jobs, err := db.ListAgentCronJobs()
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.True(t, jobs[0].Enabled, "a ticked job lands enabled")
	assert.False(t, jobs[0].OperatorAuthored, "and still without operator attribution")
	sudo, err := db.ListActiveSudoGrants(confirmConv)
	require.NoError(t, err)
	assert.Empty(t, sudo)
}

// An agent caller keeps nothing, even with the right digest and every item named.
func TestImportConfirm_AgentCallerKeepsNothing(t *testing.T) {
	f := newFlow(t)
	const caller = "import-caller"
	f.HaveGroup("home")
	f.HaveMember("home", caller)
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermGroupsImport, "human"))
	archive := confirmGroupArchive(t, "agent-import")
	asAgent := func(r *http.Request) *http.Request { return agentd.AsAgentPeer(r, caller) }
	confirm, _, _ := inspectConfirm(t, f, archive, asAgent)
	require.False(t, confirm.CanKeep)
	require.NotEmpty(t, confirm.Items, "it still sees what the file holds")

	q := url.Values{"into": {testutil.CanonicalTempDir(t)}, "keep_digest": {confirm.Digest}, "keep_all": {"true"}, "keep": confirm.ids()}
	rec := testharness.Serve(f.Mux, asAgent(httptest.NewRequest(http.MethodPost, "/v1/groups/import?"+q.Encode(), bytes.NewReader(archive))))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Only a human on this node can keep")
	assertNothingKept(t, "agent-import")
}

func assertNothingKept(t *testing.T, group string) {
	t.Helper()
	g, err := db.GetAgentGroupByName(group)
	require.NoError(t, err)
	require.NotNil(t, g)
	owners, err := db.ListAgentGroupOwners(g.ID)
	require.NoError(t, err)
	assert.Empty(t, owners)
	permissions, err := db.ListAgentGroupPermissions(g.ID)
	require.NoError(t, err)
	assert.Empty(t, permissions)
	assert.Empty(t, g.Environment)
	overrides, err := db.ListAgentPermissionOverridesForConv(confirmConv)
	require.NoError(t, err)
	assert.NotContains(t, overrides, "groups.export")
	jobs, err := db.ListAgentCronJobs()
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.False(t, jobs[0].Enabled)
}

func multipartArchive(t *testing.T, fields map[string]string, archive []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for k, v := range fields {
		require.NoError(t, form.WriteField(k, v))
	}
	part, err := form.CreateFormFile("archive", "group.zip")
	require.NoError(t, err)
	_, err = part.Write(archive)
	require.NoError(t, err)
	require.NoError(t, form.Close())
	return &body, form.FormDataContentType()
}

// An unrestricted peer's request reaches the import as this node's operator
// would, and passes as the human there. The person behind it is the peer's.
func TestImportConfirm_PeerForwardedRequestCannotClaimHuman(t *testing.T) {
	fh := newFedHarness(t)
	setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	archive := confirmGroupArchive(t, "forwarded")
	confirm, _, _ := inspectConfirm(t, fh.f, archive, agentd.AsHumanPeer)

	q := url.Values{"keep_digest": {confirm.Digest}, "keep_all": {"true"}}
	body, contentType := multipartArchive(t, map[string]string{"into": testutil.CanonicalTempDir(t)}, archive)
	r := httptest.NewRequest(http.MethodPost, "/api/groups/import?"+q.Encode(), body)
	r.Header.Set("Content-Type", contentType)
	rec := testharness.Serve(h, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "Only a human on this node can keep")
	assertNothingKept(t, "forwarded")

	body, contentType = multipartArchive(t, nil, archive)
	r = httptest.NewRequest(http.MethodPost, "/api/import/reveal?item=env:TOKENISH", body)
	r.Header.Set("Content-Type", contentType)
	rec = testharness.Serve(h, r)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), confirmSecret)
}

func TestImportConfirm_ConfirmationIsBoundToTheFile(t *testing.T) {
	f := newFlow(t)
	archive := confirmGroupArchive(t, "bound")
	confirm, _, _ := inspectConfirm(t, f, archive, agentd.AsHumanPeer)
	swapped := confirmGroupArchive(t, "swapped")
	into := testutil.CanonicalTempDir(t)

	rec, _ := importGroupWith(t, f, swapped, url.Values{"into": {into}, "keep_all": {"true"}, "keep_digest": {confirm.Digest}})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "keep_stale")

	rec, _ = importGroupWith(t, f, archive, url.Values{"into": {into}, "keep_all": {"true"}})
	require.Equal(t, http.StatusConflict, rec.Code, "keeping without having seen the digest: %s", rec.Body.String())

	for _, id := range []string{"sudo:human.notify", "sandbox", "permission:" + confirmConv + ":config.import"} {
		rec, _ = importGroupWith(t, f, archive, url.Values{"into": {into}, "keep": {id}, "keep_digest": {confirm.Digest}})
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "keep_unknown", id)
	}
	g, err := db.GetAgentGroupByName("bound")
	require.NoError(t, err)
	assert.Nil(t, g, "a refused selection imports nothing")
}

// A request that names no selection has confirmed nothing: owners land as they
// always have, and the ticks a preview shows do not.
func TestImportConfirm_NoSelectionKeepsOnlyOwners(t *testing.T) {
	f := newFlow(t)
	rec, _ := importGroupWith(t, f, confirmGroupArchive(t, "bare"), url.Values{"into": {testutil.CanonicalTempDir(t)}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	g, err := db.GetAgentGroupByName("bare")
	require.NoError(t, err)
	owners, err := db.ListAgentGroupOwners(g.ID)
	require.NoError(t, err)
	assert.Len(t, owners, 1)
	permissions, err := db.ListAgentGroupPermissions(g.ID)
	require.NoError(t, err)
	assert.Empty(t, permissions)
}

func TestImportConfirm_KeepDefaultsLeavesEnvironmentAndCron(t *testing.T) {
	f := newFlow(t)
	archive := confirmGroupArchive(t, "defaults")
	confirm, _, _ := inspectConfirm(t, f, archive, agentd.AsHumanPeer)
	rec, _ := importGroupWith(t, f, archive, url.Values{"into": {testutil.CanonicalTempDir(t)}, "keep_defaults": {"true"}, "keep_digest": {confirm.Digest}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	g, err := db.GetAgentGroupByName("defaults")
	require.NoError(t, err)
	assert.Empty(t, g.Environment)
	permissions, err := db.ListAgentGroupPermissions(g.ID)
	require.NoError(t, err)
	assert.Len(t, permissions, 1)
	jobs, err := db.ListAgentCronJobs()
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.False(t, jobs[0].Enabled)
}

func TestImportConfirm_RevealIsHumanOnlyAndAsked(t *testing.T) {
	f := newFlow(t)
	archive := confirmGroupArchive(t, "reveal")
	reveal := func(as func(*http.Request) *http.Request) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, as(httptest.NewRequest(http.MethodPost, "/v1/import/reveal?item=env:TOKENISH", bytes.NewReader(archive))))
	}
	rec := reveal(agentd.AsHumanPeer)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), confirmSecret)

	rec = reveal(func(r *http.Request) *http.Request { return agentd.AsAgentPeer(r, "some-agent") })
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), confirmSecret)
}

func TestImportConfirm_AgentFileKeepsWhatWasTicked(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("receiver")
	cwd := testutil.CanonicalTempDir(t)
	profile, err := json.Marshal(map[string]any{
		"trust_dir": true, "remote_control": true,
		"environment": []sandboxpolicy.EnvironmentEntry{{Name: "TOKENISH", Value: confirmSecret}},
	})
	require.NoError(t, err)
	b := &agentbundle.Bundle{Manifest: agentbundle.Manifest{Format: agentbundle.Format, FormatVersion: 1, Agent: agentbundle.Definition{
		Name: "portable", Harness: "claude", Profile: profile, Paths: agentbundle.Paths{Cwd: cwd},
		Permissions: []agentbundle.Permission{{Slug: "groups.export", Effect: "grant", Source: "agent"}},
	}}}
	query := "group=receiver&cwd=" + url.QueryEscape(cwd)

	rec := postAgentBundle(t, f, b, query, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), confirmSecret, "a preview never carries a value")
	var preview struct {
		Confirm confirmView `json:"confirm"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preview))
	assert.ElementsMatch(t, []string{"trust_dir", "remote_control", "env:TOKENISH", "permission:groups.export"}, preview.Confirm.ids())

	rec = postAgentBundle(t, f, b, query+"&apply=true&keep=trust_dir&keep=permission:groups.export&keep=env:TOKENISH&keep_digest="+preview.Confirm.Digest, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "remote control")
	var landed struct {
		Kept    []string `json:"kept"`
		Dropped []string `json:"dropped"`
		Spawn   struct {
			ConvID string `json:"conv_id"`
		} `json:"spawn"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &landed))
	assert.Len(t, landed.Kept, 3)
	assert.Equal(t, []string{"remote control"}, landed.Dropped)
	overrides, err := db.ListAgentPermissionOverridesForConv(landed.Spawn.ConvID)
	require.NoError(t, err)
	assert.Equal(t, db.PermEffectGrant, overrides["groups.export"], "a kept permission lands")
}

// A scope can reach beyond the imported group, so the list says what it names.
func TestImportConfirm_ListShowsAGrantsScope(t *testing.T) {
	f := newFlow(t)
	exp := &groupexport.Export{
		FormatVersion: groupexport.FormatVersion, SourceGroup: "scoped",
		Members: []groupexport.Member{{ConvID: confirmConv, Role: "lead"}},
		Convs:   []groupexport.Conv{{ConvID: confirmConv, Title: "lead", Missing: true}},
		Permissions: []groupexport.Permission{{ConvID: confirmConv, Slug: agentd.PermGroupsMembersSpawn, Effect: db.PermEffectGrant,
			ScopeJSON: `{"group":["prod"]}`, GrantedBy: "file"}},
	}
	archive, err := groupexport.Marshal(exp)
	require.NoError(t, err)
	_, _, body := inspectConfirm(t, f, archive, agentd.AsHumanPeer)
	assert.Contains(t, body, "prod")
}

func TestGroupExport_HoldsNoSudo(t *testing.T) {
	f := newFlow(t)
	const conv = "sudo-holder"
	f.HaveGroup("sudoers")
	f.HaveMember("sudoers", conv)
	now := time.Now()
	_, err := db.InsertSudoGrant(&db.SudoGrant{ConvID: conv, Slug: "human.notify", GrantedAt: now, ExpiresAt: now.Add(time.Hour), GrantedBy: "test"})
	require.NoError(t, err)
	exp, err := db.CollectGroupExport("sudoers")
	require.NoError(t, err)
	assert.Empty(t, exp.SudoGrants)
}

// fedMoveOffer offers an agent labelled as a move, asking for wider launch
// settings than a spawn here gives.
func fedMoveOffer(t *testing.T, p *fedPeer) bundletransfer.Descriptor {
	t.Helper()
	b := fedAgentBundle(t)
	b.Manifest.Agent.Profile = json.RawMessage(`{"trust_dir":true,"remote_control":true}`)
	b.SetHistory("claude-jsonl", moveSourceConv, []byte(`{"type":"user","sessionId":"`+moveSourceConv+`","cwd":"/source","message":{"content":"move history"}}`+"\n"))
	raw, err := b.Encode()
	require.NoError(t, err)
	d := bundletransfer.New(bundletransfer.Agent, raw, "Moved agent", time.Now().Add(time.Hour))
	d.Group = "receiver"
	d.Move = &bundletransfer.MoveIntent{SourceAgent: "agt_bobremote0000000000000000", SourceConv: moveSourceConv}
	env := p.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
	env.ID = d.ID
	p.send(env)
	return d
}

func TestImportConfirm_HandAcceptedOfferUsesTheList(t *testing.T) {
	for _, tc := range []struct {
		name         string
		unrestricted bool
	}{{"a restricted peer's items start unticked", false}, {"an unrestricted peer's items start ticked", true}} {
		t.Run(tc.name, func(t *testing.T) {
			fh := newFedHarness(t)
			f, p := fh.f, fh.peer
			f.HaveGroup("receiver")
			fedReceiveAgents(t, fh, "receiver")
			if tc.unrestricted {
				setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
			}
			d := fedMoveOffer(t, p)
			path := "/v1/federation/bundle-offers/" + d.ID + "/import"
			require.Equal(t, proto.AckAccepted, fedAckFor(t, p, d.ID).Status)
			cwd := testutil.CanonicalTempDir(t)

			rec := fedHuman(t, f, http.MethodPost, path, map[string]any{"cwd": cwd})
			require.Equal(t, 200, rec.Code, rec.Body.String())
			var preview struct {
				Preview struct {
					Confirm confirmView `json:"confirm"`
				} `json:"preview"`
				Confirm confirmView `json:"confirm"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preview))
			confirm := preview.Confirm
			if confirm.Digest == "" {
				confirm = preview.Preview.Confirm
			}
			require.ElementsMatch(t, []string{"trust_dir", "remote_control"}, confirm.ids())
			assert.Equal(t, d.SHA256, confirm.Digest)
			for _, item := range confirm.Items {
				assert.Equal(t, tc.unrestricted, item.Default, item.ID)
			}

			rec = fedHuman(t, f, http.MethodPost, path, map[string]any{"cwd": cwd, "apply": true, "keep": []string{"sudo"}, "keep_digest": confirm.Digest})
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "keep_unknown", "an item the offer does not hold reserves no launch")

			rec = fedHuman(t, f, http.MethodPost, path, map[string]any{"cwd": cwd, "apply": true, "keep": []string{"trust_dir"}, "keep_digest": confirm.Digest})
			require.Equal(t, 200, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), `"kept":["trusted working directory"]`)
			assert.Contains(t, rec.Body.String(), `"dropped":["remote control"]`)
		})
	}
}
