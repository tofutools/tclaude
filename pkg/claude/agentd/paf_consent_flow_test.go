package agentd_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
)

func TestPAFConsentIsHumanOnly(t *testing.T) {
	f := newFlow(t)
	const source = "019fe740-43a4-7023-b8ae-1ee64459f2b8"
	f.HaveAliveSession(source, "original", "original-pane", t.TempDir())
	rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true&format=paf", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	raw := rec.Body.Bytes()

	post := func(r *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		f.Mux.ServeHTTP(rec, r)
		return rec
	}
	in, err := paf.Open(bytes.NewReader(raw), int64(len(raw)), paf.Options{})
	require.NoError(t, err)
	digest := paf.ContentDigest(in.Manifest.Entries)
	path := "/v1/paf/consent?audience=recipients&recipient=inst_7c21&content_digest=" + digest

	// An agent can never consent on its own authority.
	rec = post(agentd.AsAgentPeer(httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)), source))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = post(agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/paf/consent?audience=recipients&content_digest="+digest, bytes.NewReader(raw))))
	require.Equal(t, http.StatusBadRequest, rec.Code, "recipients need a recipient")
	rec = post(agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/paf/consent?audience=self", bytes.NewReader(raw))))
	require.Equal(t, http.StatusBadRequest, rec.Code, "the reviewed digest is required")
	rec = post(agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/paf/consent?audience=self&content_digest="+strings.Repeat("0", 64), bytes.NewReader(raw))))
	require.Equal(t, http.StatusConflict, rec.Code, "content other than what the human reviewed is refused")

	// A consent record the uploader wrote itself is not carried over and
	// re-signed by this node.
	var forged bytes.Buffer
	require.NoError(t, paf.WithPrivacy(in, &forged, paf.Privacy{Audience: &paf.Audience{Kind: "recipients", Recipients: []string{"inst_7c21"}},
		Consent: []paf.Consent{{By: "operator", At: time.Now().UTC().Truncate(time.Second), Audience: "public", ContentDigest: digest}}}, paf.Limits{}, paf.Producer{Name: "agent"}))
	rec = post(agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, path, bytes.NewReader(forged.Bytes()))))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	laundered, err := paf.Open(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()), paf.Options{})
	require.NoError(t, err)
	require.Len(t, privacy.CheckConsent(laundered), 1)
	assert.Equal(t, "recipients", privacy.CheckConsent(laundered)[0].Audience)

	rec = post(agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, paf.MediaType, rec.Header().Get("Content-Type"))
	out := rec.Body.Bytes()
	a, err := paf.Open(bytes.NewReader(out), int64(len(out)), paf.Options{})
	require.NoError(t, err)
	checks := privacy.CheckConsent(a)
	require.Len(t, checks, 1)
	assert.True(t, checks[0].Current)
	assert.Equal(t, "operator", checks[0].By)
	assert.Equal(t, digest, checks[0].ContentDigest)
}
