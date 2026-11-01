package agentd_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/pwf"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

const envelopeReviewer = "019fe740-43a4-7023-b8ae-1ee64459f2b7"

type envelopeView struct {
	Title, Refused, ReviewDigest, RootID string
	Applied                              bool
	Envelope                             *struct {
		Workflow   string
		Workflows  []struct{ Key, Root string }
		Performers []struct {
			Role, Target, AgentKey, Name, Harness string
			History                               bool
		}
		CarriedBy []string
	}
}

// envelopeFlow has the two templates stored and an agent to ship.
func envelopeFlow(t *testing.T) *testharness.Flow {
	t.Helper()
	f, _ := processAuthoringFlow(t)
	for id, source := range map[string]string{"pwf-review": pwfChildSource, "pwf-ship": pwfRootSource} {
		rec := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/"+id, map[string]any{"source": source})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}
	f.HaveAliveSession(envelopeReviewer, "reviewer", "reviewer-pane", testutil.CanonicalTempDir(t))
	return f
}

func openPAF(t *testing.T, raw []byte) *paf.Archive {
	t.Helper()
	a, err := paf.Open(bytes.NewReader(raw), int64(len(raw)), paf.Options{Purpose: paf.PurposeImport})
	require.NoError(t, err)
	return a
}

func envelopeReview(t *testing.T, f *testharness.Flow, file []byte, query string) envelopeView {
	t.Helper()
	rec := pwfRaw(t, f, http.MethodPost, "/v1/process/import?"+query, file)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view envelopeView
	testharness.DecodeJSON(t, rec, &view)
	return view
}

func deleteTemplates(t *testing.T, f *testharness.Flow, ids ...string) {
	t.Helper()
	for _, id := range ids {
		rec := processTemplateRequest(t, f, http.MethodDelete, "/v1/process/templates/"+id, nil)
		require.Less(t, rec.Code, 300, rec.Body.String())
	}
}

// A workflow ships an agent for one of its roles. Importing the workflow stores
// templates and lands nobody. Landing the performer is a separate agent import
// of the same file, under the role the importer names.
func TestWorkflowEnvelopeShipsAPerformerOptIn(t *testing.T) {
	f := envelopeFlow(t)
	receiver := f.HaveGroup("receiver")

	rec := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-ship/export?performer=reviewer="+envelopeReviewer, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, paf.MediaType, rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "pwf-ship.paf")
	file := rec.Body.Bytes()

	a := openPAF(t, file)
	assert.Equal(t, paf.KindBundle, a.Manifest.Kind)
	require.Len(t, a.Manifest.Agents, 1)
	key := a.Manifest.Agents[0]
	require.Len(t, a.Performers("pwf-ship"), 1)
	ag, _ := a.Agent(key)
	assert.Nil(t, ag.Native, "the conversation stays home unless asked for")

	deleteTemplates(t, f, "pwf-ship", "pwf-review")

	view := envelopeReview(t, f, file, "")
	require.NotNil(t, view.Envelope)
	assert.Equal(t, "pwf-ship", view.Envelope.Workflow)
	require.Len(t, view.Envelope.Performers, 1)
	assert.Equal(t, "reviewer", view.Envelope.Performers[0].Role)
	assert.Equal(t, key, view.Envelope.Performers[0].AgentKey)
	assert.False(t, view.Envelope.Performers[0].History)

	mapped := envelopeReview(t, f, file, "map=reviewer=qa")
	assert.Equal(t, "qa", mapped.Envelope.Performers[0].Target, "the review shows the role it would land under")

	applied := pwfRaw(t, f, http.MethodPost, "/v1/process/import?apply=1&review="+view.ReviewDigest, file)
	require.Equal(t, http.StatusOK, applied.Code, applied.Body.String())
	got := processTemplateRequest(t, f, http.MethodGet, "/v1/process/templates/pwf-review", nil)
	require.Equal(t, http.StatusOK, got.Code, "the subprocess template came with it")
	members, err := db.ListAgentGroupMembers(receiver.ID)
	require.NoError(t, err)
	assert.Empty(t, members, "importing the workflow landed nobody")

	dest := testutil.CanonicalTempDir(t)
	query := "apply=true&group=receiver&name=shipped&role=qa&cwd=" + url.QueryEscape(dest)
	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/agent-bundle/import?"+query, bytes.NewReader(file))))
	require.Equal(t, http.StatusBadRequest, rec.Code, "a bundle needs the agent named: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "--agent")

	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/agent-bundle/import?agent="+key+"&"+query, bytes.NewReader(file))))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var landed struct {
		Agent struct {
			Role string `json:"role"`
		} `json:"agent"`
		Spawn struct {
			ConvID string `json:"conv_id"`
		} `json:"spawn"`
		CarriedWorkflows []paf.WorkflowItem `json:"carried_workflows"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &landed))
	assert.Equal(t, "qa", landed.Agent.Role)
	assert.NotEqual(t, envelopeReviewer, landed.Spawn.ConvID, "an import forks")
	require.Len(t, landed.CarriedWorkflows, 1)

	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/agent-bundle/import?agent="+key+"&cwd="+url.QueryEscape(dest), bytes.NewReader(file))))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var unnamed struct {
		Agent struct {
			Role string `json:"role"`
		} `json:"agent"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &unnamed))
	assert.Empty(t, unnamed.Agent.Role, "the file proposes a role and cannot assign one")
}

// An agent asking the human to approve a .paf import shows the review of the
// workflow inside, and what else the file ships.
func TestWorkflowEnvelopePopupShowsThePerformers(t *testing.T) {
	f := envelopeFlow(t)
	rec := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-ship/export?performer=reviewer="+envelopeReviewer, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	file := rec.Body.Bytes()

	const conv = "pwf-envelope-popup-bbbb-000000000001"
	f.HaveAliveSession(conv, "pwf-env-popup", "tmux-pwf-env-popup", f.TestCwd("popup"))
	_, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	popups, restore := agentd.StubCapturingApprovalForTest(true)
	t.Cleanup(restore)
	req := httptest.NewRequest(http.MethodPost, "/v1/process/import", bytes.NewReader(file))
	req.Header.Set("Content-Type", paf.MediaType)
	req.Header.Set("X-Tclaude-Ask-Human", "30s")
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(req, conv))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, *popups, 1)
	body := (*popups)[0].Body
	assert.Contains(t, body, "The file also ships 1 agent(s); importing the workflow lands none of them.")
	assert.Contains(t, body, "Agents shipped for its roles")
	assert.NotContains(t, body, "cannot be reviewed")
}

// The export gates of both sides apply: a role the workflow does not ask for,
// an agent the caller may not export, and credentials in what would travel.
func TestWorkflowEnvelopeExportGates(t *testing.T) {
	f := envelopeFlow(t)

	rec := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-ship/export?performer=nobody="+envelopeReviewer, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `no role \"nobody\"`)

	const caller = "aaaaaaaa-1111-2222-3333-444444444444"
	f.HaveAliveSession(caller, "caller", "caller-pane", testutil.CanonicalTempDir(t))
	r := httptest.NewRequest(http.MethodGet, "/v1/process/templates/pwf-ship/export?performer=reviewer="+envelopeReviewer, nil)
	require.NoError(t, db.SetAgentPermissionOverride(caller, agentd.PermProcessTemplatesRead, db.PermEffectGrant, "test"))
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(r, caller))
	require.Equal(t, http.StatusForbidden, rec.Code, "reading templates is not exporting an agent: %s", rec.Body.String())

	secret := "ghp_" + strings.Repeat("a1b2", 9)
	source := strings.Replace(strings.ReplaceAll(pwfChildSource, "pwf-review", "pwf-leak"), "Review it", "Use "+secret, 1)
	saved := processTemplateRequest(t, f, http.MethodPost, "/v1/process/templates/pwf-leak", map[string]any{"source": source})
	require.Equal(t, http.StatusCreated, saved.Code, saved.Body.String())
	rec = pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-leak/export?performer=reviewer="+envelopeReviewer, nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "flagged_credentials")
	assert.NotContains(t, rec.Body.String(), secret)
	rec = pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-leak/export?allow_flagged=true&performer=reviewer="+envelopeReviewer, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, openPAF(t, rec.Body.Bytes()).Privacy().Scan.Allowed)
}

// An agent carries the workflows it was asked to, under the export permission
// for workflows as well as for itself. The agent import ignores them, and a
// workflow import reads them out of the same file with the same review, so the
// unsigned policy holds there too.
func TestAgentCarriesWorkflows(t *testing.T) {
	f := envelopeFlow(t)
	export := "/v1/agent-bundle/export?agent=" + envelopeReviewer

	rec := pwfRaw(t, f, http.MethodGet, export+"&workflow=pwf-missing", nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = pwfRaw(t, f, http.MethodGet, export+"&workflow=pwf-ship&format=v1", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	r := httptest.NewRequest(http.MethodGet, export+"&workflow=pwf-ship", nil)
	require.NoError(t, db.SetAgentPermissionOverride(envelopeReviewer, agentd.PermAgentBundleExport, db.PermEffectGrant, "test"))
	rec = testharness.Serve(f.Mux, agentd.AsAgentPeer(r, envelopeReviewer))
	require.Equal(t, http.StatusForbidden, rec.Code, "exporting itself is not exporting templates: %s", rec.Body.String())

	rec = pwfRaw(t, f, http.MethodGet, export+"&workflow=pwf-ship&workflow=pwf-review", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	file := rec.Body.Bytes()
	a := openPAF(t, file)
	assert.Equal(t, paf.KindAgent, a.Manifest.Kind)
	assert.Equal(t, paf.EnvelopeVersion, a.Manifest.MinReaderVersion)
	require.Len(t, a.Manifest.Workflows, 2)
	require.Len(t, a.Manifest.Links, 2)

	view := envelopeReview(t, f, file, "workflow=pwf-review")
	assert.Equal(t, "pwf-review", view.Envelope.Workflow)
	require.Len(t, view.Envelope.Workflows, 2)
	assert.Equal(t, a.Manifest.Agents, view.Envelope.CarriedBy)
	assert.Empty(t, view.Envelope.Performers)

	other := pwfRaw(t, f, http.MethodPost, "/v1/process/import?apply=1&review="+view.ReviewDigest, file)
	require.Equal(t, http.StatusConflict, other.Code, "a review of one workflow does not apply another: %s", other.Body.String())

	missing := pwfRaw(t, f, http.MethodPost, "/v1/process/import?workflow=nope", file)
	require.Equal(t, http.StatusUnprocessableEntity, missing.Code, missing.Body.String())

	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":true},"processes":{"refuse_unsigned_imports":true}}`), 0o644))
	view = envelopeReview(t, f, file, "")
	assert.Contains(t, view.Refused, "without a valid signature", "the policy reads the envelope's signature")
}

// The envelope's signature is the workflow's: a signed .paf passes the policy,
// and the review names the key.
func TestWorkflowEnvelopeSignaturePassesThePolicy(t *testing.T) {
	f := envelopeFlow(t)
	var buf bytes.Buffer
	w := paf.NewWriter(&buf, paf.KindAgent, paf.Limits{})
	require.NoError(t, w.AddAgent("reviewer", paf.Agent{Name: "reviewer"}, paf.Environment{Harness: paf.Harness{Name: "claude"}}))
	_, ref, err := pwf.ParseTemplate([]byte(pwfChildSource))
	require.NoError(t, err)
	require.NoError(t, pwf.WriteEmbedded(w, "pwf-review", pwf.Bundle{Root: ref, Templates: []pwf.Template{{Ref: ref, Head: true, Source: []byte(pwfChildSource)}}}))
	key := paf.SigningKey{Key: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32)), KeyID: "someone", Signer: &paf.Signer{Kind: "instance"}}
	_, err = w.Close(paf.Producer{Name: "test"}, key)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":true},"processes":{"refuse_unsigned_imports":true}}`), 0o644))
	view := envelopeReview(t, f, buf.Bytes(), "")
	assert.Empty(t, view.Refused)
}

// With processes off an export cannot carry workflows.
func TestCarriedWorkflowsNeedProcessesOn(t *testing.T) {
	f := envelopeFlow(t)
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":false}}`), 0o644))
	rec := pwfRaw(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+envelopeReviewer+"&workflow=pwf-ship", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "processes_disabled")
}

// A group carries its saved processes. Importing the group stores none of them
// and says they are there; a workflow import takes them from the same file.
func TestGroupCarriesItsSavedProcesses(t *testing.T) {
	f := envelopeFlow(t)
	const conv = "cccccccc-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "carol")
	f.HaveAliveSession(conv, "lbl-c", "tmux-c", f.TestCwd("work"))
	group := f.HaveGroup("crew")
	f.HaveMemberWithRole("crew", conv, "lead")
	require.NoError(t, db.PinProcessToGroup(group.ID, "pwf-ship"))

	plain := pwfRaw(t, f, http.MethodGet, "/v1/groups/crew/export", nil)
	require.Equal(t, http.StatusOK, plain.Code, plain.Body.String())
	assert.Empty(t, openPAF(t, plain.Body.Bytes()).Manifest.Workflows, "opt-in")

	rec := pwfRaw(t, f, http.MethodGet, "/v1/groups/crew/export?workflows=own", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	file := rec.Body.Bytes()
	a := openPAF(t, file)
	assert.Equal(t, paf.KindGroup, a.Manifest.Kind)
	require.Len(t, a.Manifest.Workflows, 1)

	insp := testharness.Serve(f.Mux, agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/groups/import/inspect?as=crew2", bytes.NewReader(file))))
	require.Equal(t, http.StatusOK, insp.Code, insp.Body.String())
	var preview struct {
		CarriedWorkflows []paf.WorkflowItem `json:"carried_workflows"`
	}
	require.NoError(t, json.Unmarshal(insp.Body.Bytes(), &preview))
	require.Len(t, preview.CarriedWorkflows, 1)

	view := envelopeReview(t, f, file, "")
	assert.Equal(t, "Ship", view.Title)

	rec = pwfRaw(t, f, http.MethodGet, "/v1/groups/crew/export?workflows=own&format=zip", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// A snapshot carries workflows too, and its spawn path ignores them.
func TestSnapshotCarriesWorkflows(t *testing.T) {
	f := envelopeFlow(t)
	rec := profileReq(t, f, http.MethodPost, "/v1/snapshots", map[string]any{"agent": envelopeReviewer, "name": "with-flow", "workflow": []string{"pwf-review"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = pwfRaw(t, f, http.MethodGet, "/v1/snapshots/with-flow/export", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	a := openPAF(t, rec.Body.Bytes())
	require.Len(t, a.Manifest.Workflows, 1)
}

// Sudo is temporary, so a shipped performer holds none of it.
func TestShippedPerformerCarriesNoSudo(t *testing.T) {
	f := envelopeFlow(t)
	_, err := db.InsertSudoGrant(&db.SudoGrant{ConvID: envelopeReviewer, Slug: "agent.spawn", ExpiresAt: time.Now().Add(time.Hour), GrantedAt: time.Now(), Reason: "test", GrantedBy: "human"})
	require.NoError(t, err)
	rec := pwfRaw(t, f, http.MethodGet, "/v1/process/templates/pwf-ship/export?performer=reviewer="+envelopeReviewer, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	a := openPAF(t, rec.Body.Bytes())
	raw, err := a.ReadEntry(paf.AgentPath(a.Manifest.Agents[0], "agent.json"), 1<<20)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "sudo")
	assert.NotContains(t, string(raw), "agent.spawn")
}
