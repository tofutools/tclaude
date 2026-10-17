package agentd_test

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type joinView struct {
	HubURL    string           `json:"hub_url"`
	Space     string           `json:"space"`
	Invite    bool             `json:"invite"`
	Expired   bool             `json:"expired"`
	CA        []proto.CertInfo `json:"ca"`
	Joined    bool             `json:"joined"`
	HubCAFile string           `json:"hub_ca_file"`
	Current   struct {
		HubURL  string `json:"hub_url"`
		Enabled bool   `json:"enabled"`
	} `json:"current"`
}

// tlsHub serves an in-process hub over TLS with httptest's self-signed
// certificate, as a hub with a private certificate would be.
func tlsHub(t *testing.T) (*hub.Store, string, string) {
	t.Helper()
	st, err := hub.OpenStore(filepath.Join(t.TempDir(), "hub.sqlite"))
	require.NoError(t, err)
	h, err := hub.New(st, hub.Config{})
	require.NoError(t, err)
	srv := httptest.NewTLSServer(h.Handler())
	t.Cleanup(func() { h.Close(); srv.Close(); _ = st.Close() })
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	return st, "wss" + strings.TrimPrefix(srv.URL, "https"), ca
}

func TestFederationJoinCode_JoinsATLSHubWithItsCAAndInvite(t *testing.T) {
	f := newFlow(t)
	agentd.ResetFederationForTest()
	t.Cleanup(agentd.ResetFederationForTest)
	st, url, ca := tlsHub(t)
	invite, err := st.CreateInvite("home", time.Hour)
	require.NoError(t, err)
	code, err := proto.EncodeJoinCode(proto.JoinCode{Hub: url, Invite: invite, Space: "home", Expires: time.Now().Add(time.Hour), CA: ca})
	require.NoError(t, err)

	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/join", map[string]any{"code": code, "preview": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var preview joinView
	testharness.DecodeJSON(t, rec, &preview)
	require.Equal(t, url, preview.HubURL)
	require.Equal(t, "home", preview.Space)
	require.True(t, preview.Invite)
	require.False(t, preview.Expired)
	require.Len(t, preview.CA, 1)
	require.True(t, preview.CA[0].SelfSigned)
	require.Empty(t, preview.Current.HubURL)
	require.False(t, preview.Joined)
	require.Nil(t, fedStatus(t, f).Hub, "a preview changes nothing")

	// A pasted code is often wrapped; the daemon takes it as pasted.
	wrapped := code[:40] + "\n" + code[40:]
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/join", map[string]any{"code": wrapped, "name": "laptop"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var joined joinView
	testharness.DecodeJSON(t, rec, &joined)
	require.True(t, joined.Joined)
	stored, err := os.ReadFile(joined.HubCAFile)
	require.NoError(t, err)
	require.Equal(t, ca, string(stored), "the code's CA is what the daemon trusts")
	info, err := os.Stat(joined.HubCAFile)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	fedEventually(t, "the daemon connects over TLS with the code's CA", func() bool {
		s := fedStatus(t, f)
		return s.Hub != nil && s.Hub.State == "connected"
	})
	invites, err := st.ListInvites()
	require.NoError(t, err)
	require.Len(t, invites, 1)
	require.Equal(t, agentd.FederationInstanceIDForTest(), invites[0].UsedBy, "the invite admitted this node")

	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/join", map[string]any{"code": code, "preview": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	testharness.DecodeJSON(t, rec, &preview)
	require.Equal(t, url, preview.Current.HubURL, "a later preview names the hub it would replace")
	require.True(t, preview.Current.Enabled)
}

func TestFederationJoinCode_RefusesExpiredMangledAndUnsafeCodes(t *testing.T) {
	f := newFlow(t)
	agentd.ResetFederationForTest()
	t.Cleanup(agentd.ResetFederationForTest)
	expired, err := proto.EncodeJoinCode(proto.JoinCode{Hub: "wss://hub.example", Invite: "tchi_x", Expires: time.Now().Add(-time.Minute)})
	require.NoError(t, err)
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/join", map[string]any{"code": expired, "preview": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var preview joinView
	testharness.DecodeJSON(t, rec, &preview)
	require.True(t, preview.Expired, "a preview says it expired")
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/join", map[string]any{"code": expired})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "join_code_expired")

	plain, err := proto.EncodeJoinCode(proto.JoinCode{Hub: "ws://hub.example:8470", Invite: "tchi_x"})
	require.NoError(t, err)
	for name, code := range map[string]string{"invite": "tchi_abc", "mangled": expired[:30], "plain ws off loopback": plain} {
		rec = fedHuman(t, f, http.MethodPost, "/v1/federation/join", map[string]any{"code": code})
		require.Equal(t, http.StatusBadRequest, rec.Code, name)
		require.Contains(t, rec.Body.String(), "invalid_join_code", name)
	}
	require.Nil(t, fedStatus(t, f).Hub, "nothing was configured")
}

func TestFederationJoinCode_AHubInviteMintedThroughTheDashboardComesAsAJoinCode(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	handler := agentd.BuildDashboardHandlerForTest()
	call := func(method, tail string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return testharness.Serve(handler, testharness.JSONRequest(t, method, "/api/federation/"+tail, body))
	}
	token, err := fh.store.PrepareAdminClaim(false, time.Now())
	require.NoError(t, err)
	_ = call("GET", "hub/status", nil) // learn the new admin generation
	require.Equal(t, 200, call("GET", "hub/status", nil).Code)
	rec := call("POST", "hub/claim", map[string]any{"token": token})
	require.Equal(t, 200, rec.Code, rec.Body.String())

	rec = call("POST", "hub/invites", map[string]any{"space": "home", "ttl_seconds": 3600})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var created struct {
		Token    string `json:"token"`
		JoinCode string `json:"join_code"`
	}
	testharness.DecodeJSON(t, rec, &created)
	code, err := proto.DecodeJoinCode(created.JoinCode)
	require.NoError(t, err)
	require.Equal(t, fh.url, code.Hub, "the address this node reaches the hub at")
	require.Equal(t, created.Token, code.Invite)
	require.Equal(t, "home", code.Space)
	require.WithinDuration(t, time.Now().Add(time.Hour), code.Expires, time.Minute)
	require.Empty(t, code.CA, "this node trusts the system roots, so the code does too")

	// The dashboard previews the same code it would join with.
	rec = call("POST", "join", map[string]any{"code": created.JoinCode, "preview": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
}
