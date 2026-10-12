package agentd_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

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
	path := "/v1/paf/consent?audience=recipients&recipient=inst_7c21"

	// An agent can never consent on its own authority.
	rec = post(agentd.AsAgentPeer(httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)), source))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	rec = post(agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/paf/consent?audience=recipients", bytes.NewReader(raw))))
	require.Equal(t, http.StatusBadRequest, rec.Code, "recipients need a recipient")

	rec = post(agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, paf.MediaType, rec.Header().Get("Content-Type"))
	out := rec.Body.Bytes()
	in, err := paf.Open(bytes.NewReader(raw), int64(len(raw)), paf.Options{})
	require.NoError(t, err)
	a, err := paf.Open(bytes.NewReader(out), int64(len(out)), paf.Options{})
	require.NoError(t, err)
	checks := privacy.CheckConsent(a)
	require.Len(t, checks, 1)
	assert.True(t, checks[0].Current)
	assert.Equal(t, "operator", checks[0].By)
	assert.Equal(t, paf.ContentDigest(in.Manifest.Entries), checks[0].ContentDigest)
}
