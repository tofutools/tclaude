package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestPAFPublishFlow(t *testing.T) {
	f := newFlow(t)
	const source = "019fe740-43a4-7023-b8ae-1ee64459f2f1"
	const other = "019fe740-43a4-7023-b8ae-1ee64459f2f2"
	f.HaveAliveSession(source, "original", "original-pane", testutil.CanonicalTempDir(t))
	f.HaveAliveSession(other, "bystander", "bystander-pane", testutil.CanonicalTempDir(t))
	serve := func(r *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		f.Mux.ServeHTTP(rec, r)
		return rec
	}
	var body bytes.Buffer
	require.NoError(t, json.NewEncoder(&body).Encode(map[string]any{"agent": source, "name": "to-publish"}))
	rec := serve(agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/snapshots", &body)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = serve(agentd.AsHumanPeer(httptest.NewRequest(http.MethodGet, "/v1/snapshots/to-publish/export", nil)))
	require.Equal(t, http.StatusOK, rec.Code)
	src := rec.Body.Bytes()
	publish := func(query string, asAgent string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/paf/publish"+query, bytes.NewReader(src))
		if asAgent != "" {
			return serve(agentd.AsAgentPeer(r, asAgent))
		}
		return serve(agentd.AsHumanPeer(r))
	}

	// An agent can never publish.
	assert.Equal(t, http.StatusForbidden, publish("", other).Code)

	rec = publish("?drop_reasoning=true", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var preview struct {
		Report  privacy.RedactReport   `json:"report"`
		Summary privacy.ConsentSummary `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preview))
	kinds := map[string]bool{}
	for _, r := range preview.Report.Redactions {
		kinds[r.Kind] = true
	}
	assert.True(t, kinds[privacy.RedactStripIdentity], "identity is always stripped: %v", kinds)
	require.NotEmpty(t, preview.Summary.Digest)

	assert.Equal(t, http.StatusConflict, publish("?drop_reasoning=true&content_digest="+strings.Repeat("0", 64), "").Code)

	published := func() *paf.Archive {
		t.Helper()
		rec := publish("?drop_reasoning=true&content_digest="+preview.Summary.Digest, "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		raw := rec.Body.Bytes()
		a, err := paf.Open(bytes.NewReader(raw), int64(len(raw)), paf.Options{})
		require.NoError(t, err)
		return a
	}
	a := published()
	require.Len(t, a.Signatures, 1)
	assert.Equal(t, paf.SignatureValid, a.Signatures[0].Status)
	assert.Equal(t, "publisher", a.Signatures[0].Signature.Signer.Kind)
	assert.True(t, strings.HasPrefix(a.Signatures[0].Signature.KeyID, "pub_"))
	ag, ok := a.Agent(a.Manifest.Agents[0])
	require.True(t, ok)
	assert.Empty(t, ag.ID)
	assert.Nil(t, ag.Instance)
	assert.Nil(t, ag.Native)
	for _, l := range ag.Lineage {
		assert.Nil(t, l.From)
	}
	pr := a.Privacy()
	require.NotNil(t, pr)
	require.Len(t, pr.Consent, 1)
	assert.Equal(t, privacy.AudiencePublic, pr.Consent[0].Audience)
	assert.Equal(t, preview.Summary.Digest, pr.Consent[0].ContentDigest)
	for _, e := range a.Manifest.Entries {
		raw, err := a.ReadEntry(e.Path, 1<<20)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), source, "%s names the source conversation", e.Path)
		assert.NotContains(t, string(raw), agentd.FederationInstanceIDForTest(), "%s names this node", e.Path)
	}

	// Publications from this node share one pseudonymous key.
	again := published()
	assert.Equal(t, a.Signatures[0].Signature.PublicKey, again.Signatures[0].Signature.PublicKey)
}
