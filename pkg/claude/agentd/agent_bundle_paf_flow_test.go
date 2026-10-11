package agentd_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/legacy"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestAgentBundlePAFExportImport(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("receiver")
	const source = "019fe740-43a4-7023-b8ae-1ee64459f2b7"
	cwd := testutil.CanonicalTempDir(t)
	f.HaveAliveSession(source, "original", "original-pane", cwd)

	rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&format=nope", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	rec = profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true&format=paf", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, paf.MediaType, rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "agent.paf")
	raw := rec.Body.Bytes()
	a, err := paf.Open(bytes.NewReader(raw), int64(len(raw)), paf.Options{Purpose: paf.PurposeImport})
	require.NoError(t, err)
	require.Equal(t, paf.KindAgent, a.Manifest.Kind)
	require.Len(t, a.Manifest.Agents, 1)
	ag, ok := a.Agent(a.Manifest.Agents[0])
	require.True(t, ok)
	require.NotNil(t, ag.Native)
	// The neutral context travels beside the native history.
	tr, err := paf.ReadTranscript(a, a.Manifest.Agents[0], paf.ScopeEffective, paf.ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, "claude", tr.Source.Harness)

	dest := testutil.CanonicalTempDir(t)
	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost,
		"/v1/agent-bundle/import?apply=true&group=receiver&name=paf-agent&cwd="+url.QueryEscape(dest), bytes.NewReader(raw)))
	rec = httptest.NewRecorder()
	f.Mux.ServeHTTP(rec, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var result struct {
		Applied bool `json:"applied"`
		History bool `json:"history"`
		Spawn   struct {
			ConvID string `json:"conv_id"`
		} `json:"spawn"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	require.True(t, result.Applied)
	require.NotEmpty(t, result.Spawn.ConvID)
	assert.NotEqual(t, source, result.Spawn.ConvID)
	h, _ := harness.Get("claude")
	imported, err := h.History.Export(result.Spawn.ConvID, dest)
	require.NoError(t, err)
	assert.Contains(t, string(imported), dest)
}

// A peer that advertises paf_agent_bundles receives agent transfers as a
// signed PAF; every other peer keeps receiving bundle v1.
func TestFederation_AgentSharingFormatFollowsPeerCatalog(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveGroup("source")
	const source = "019fe740-43a4-7023-b8ae-1ee64459f2c3"
	cwd := testutil.CanonicalTempDir(t)
	f.HaveAliveSession(source, "share-source", "share-source-pane", cwd)
	f.HaveMember("source", source)
	require.NoError(t, db.GrantAgentPermissionWithScope(source, "agent.share", fmt.Sprintf(`{"peer":["%s/destination"]}`, p.id.ID()), "human"))
	offer := func(n int) legacy.Info {
		t.Helper()
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/share-agent", map[string]any{"peer": "bob", "group": "destination", "agent": "self", "history": true}), source))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		fedEventually(t, "agent offer", func() bool { return len(p.envelopes(proto.KindBundleOffer)) >= n })
		var d bundletransfer.Descriptor
		require.NoError(t, p.envelopes(proto.KindBundleOffer)[n-1].DecodePayload(&d))
		b, info, err := legacy.DecodeBundleBytes(d.Inline, legacy.DecodeLimits{MaxBytes: 64 << 20}, "")
		require.NoError(t, err)
		require.NotNil(t, b.Manifest.History)
		return info
	}
	require.NoError(t, db.PutFederationCatalog(p.id.ID(), `{}`, time.Now()))
	assert.Equal(t, legacy.SourceAgentBundleV1, offer(1).Source)

	require.NoError(t, db.PutFederationCatalog(p.id.ID(), `{"paf_agent_bundles":true}`, time.Now()))
	info := offer(2)
	assert.Equal(t, paf.Format, info.Source)
	require.Len(t, info.Signatures, 1)
	assert.Equal(t, paf.SignatureValid, info.Signatures[0].Status)
	assert.True(t, info.HasContext)
}

// An agent offer that arrives as a PAF is accepted and can be inspected
// entry by entry before anyone imports it.
func TestFederation_PAFAgentOfferContents(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	fh.f.HaveGroup("receiver")
	fedReceiveAgents(t, fh, "receiver")
	b := fedAgentBundle(t)
	b.SetHistory("jsonl", "source-conversation", []byte("{\"text\":\"hello from the source\"}\n"))
	var buf bytes.Buffer
	require.NoError(t, legacy.WriteAgentBundle(b, &buf, legacy.WriteOptions{Producer: paf.Producer{Name: "tclaude", Version: "test"}}))
	d := bundletransfer.New(bundletransfer.Agent, buf.Bytes(), "Shared agent", time.Now().Add(time.Hour))
	d.Group = "receiver"
	env := fh.peer.envelope(proto.KindBundleOffer, proto.Endpoint{}, d)
	env.ID = d.ID
	fh.peer.send(env)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)

	h := agentd.BuildDashboardHandlerForTest()
	base := "/api/federation/bundle-offers/" + d.ID + "/contents?peer=" + fh.peer.id.ID()
	rec := testharness.Serve(h, testharness.JSONRequest(t, http.MethodGet, base, nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listing struct {
		Entries []struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		} `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listing))
	native := ""
	paths := map[string]string{}
	for _, e := range listing.Entries {
		paths[e.Path] = e.Kind
		if e.Kind == "jsonl" {
			native = e.Path
		}
	}
	assert.Equal(t, "json", paths[paf.ManifestPath])
	require.NotEmpty(t, native)
	rec = testharness.Serve(h, testharness.JSONRequest(t, http.MethodGet, base+"&path="+url.QueryEscape(native), nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "hello from the source")
}
