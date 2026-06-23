package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/remoteaccess"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// raInfo mirrors the fields of agentd.remoteAccessInfo the tests assert on.
type raInfo struct {
	MaterialExists bool     `json:"material_exists"`
	Enabled        bool     `json:"enabled"`
	Bind           string   `json:"bind"`
	CAPresent      bool     `json:"ca_present"`
	SANs           []string `json:"sans"`
	Clients        []struct {
		Name   string `json:"name"`
		HasP12 bool   `json:"has_p12"`
	} `json:"clients"`
}

func setupTestMaterial(t *testing.T) {
	t.Helper()
	_, err := remoteaccess.Setup(remoteaccess.SetupOptions{
		Bind:        "0.0.0.0:8443",
		Passphrase:  "pp-pp-pp-pp",
		ClientName:  "phone",
		P12Password: "p12pw",
	})
	require.NoError(t, err, "remoteaccess.Setup")
}

// Scenario: the Config tab's cert-management panel reads /api/remote-access/info
// for material state, server-cert SANs and the device list. Before setup it
// reports no material; after setup it reflects the CA, the seeded SANs, and the
// first device.
func TestRemoteAccessAdmin_Info(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		_ = newFlow(t) // temp $HOME
		mux := agentd.BuildDashboardHandlerForTest()

		rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/remote-access/info", nil))
		require.Equal(t, http.StatusOK, rec.Code, "info body=%s", rec.Body.String())
		var before raInfo
		testharness.DecodeJSON(t, rec, &before)
		assert.False(t, before.MaterialExists, "no material before setup")

		setupTestMaterial(t)

		rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/remote-access/info", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		var after raInfo
		testharness.DecodeJSON(t, rec, &after)
		assert.True(t, after.MaterialExists, "material present after setup")
		assert.True(t, after.CAPresent, "CA present after setup")
		assert.Contains(t, after.SANs, "localhost", "server cert SANs seeded with localhost")
		require.Len(t, after.Clients, 1, "one device after setup")
		assert.Equal(t, "phone", after.Clients[0].Name)
		assert.True(t, after.Clients[0].HasP12, "first device has a downloadable .p12")
	})
}

// Scenario: add a device from the UI, then download its .p12. A bogus device
// name is rejected (path-traversal guard).
func TestRemoteAccessAdmin_AddClientAndDownload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		_ = newFlow(t)
		setupTestMaterial(t)
		mux := agentd.BuildDashboardHandlerForTest()

		add := testharness.JSONRequest(t, http.MethodPost, "/api/remote-access/add-client",
			map[string]string{"name": "tablet", "p12_password": "p12pw2"})
		rec := testharness.Serve(mux, add)
		require.Equal(t, http.StatusOK, rec.Code, "add-client body=%s", rec.Body.String())

		// The new device's .p12 downloads as an attachment.
		dl := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/remote-access/client?name=tablet", nil))
		require.Equal(t, http.StatusOK, dl.Code, "client download body=%s", dl.Body.String())
		assert.Equal(t, "application/x-pkcs12", dl.Header().Get("Content-Type"))
		assert.Contains(t, dl.Header().Get("Content-Disposition"), "tablet.p12")
		assert.NotEmpty(t, dl.Body.Bytes(), ".p12 download is non-empty")

		// A traversal name is refused before touching the filesystem.
		bad := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/remote-access/client?name=../../etc/passwd", nil))
		assert.Equal(t, http.StatusBadRequest, bad.Code, "traversal name must be rejected")
	})
}

// Scenario: add a public/extra host name from the UI; the server cert is
// reissued (under the existing CA) covering it, cumulatively with the seeded
// SANs.
func TestRemoteAccessAdmin_AddHosts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		_ = newFlow(t)
		setupTestMaterial(t)
		mux := agentd.BuildDashboardHandlerForTest()

		rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/remote-access/add-hosts",
			map[string]string{"hosts": "example.com, myhost.tailnet.ts.net"}))
		require.Equal(t, http.StatusOK, rec.Code, "add-hosts body=%s", rec.Body.String())

		info := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/remote-access/info", nil))
		var got raInfo
		testharness.DecodeJSON(t, info, &got)
		assert.Contains(t, got.SANs, "example.com")
		assert.Contains(t, got.SANs, "myhost.tailnet.ts.net")
		assert.Contains(t, got.SANs, "localhost", "original SANs preserved")
	})
}

// Scenario: first-time setup driven entirely from the UI — no material yet, the
// setup endpoint generates it and (enable=true) flips remote_access on.
func TestRemoteAccessAdmin_SetupFromUI(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		_ = newFlow(t)
		mux := agentd.BuildDashboardHandlerForTest()

		body := map[string]any{
			"bind":         "0.0.0.0:8443",
			"passphrase":   "pp-pp-pp-pp",
			"p12_password": "p12pw",
			"client_name":  "phone",
			"enable":       true,
		}
		rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/remote-access/setup", body))
		require.Equal(t, http.StatusOK, rec.Code, "setup body=%s", rec.Body.String())

		info := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/remote-access/info", nil))
		var got raInfo
		testharness.DecodeJSON(t, info, &got)
		assert.True(t, got.MaterialExists, "setup generated material")
		assert.True(t, got.Enabled, "enable=true flipped remote_access on in config")
		assert.Equal(t, "0.0.0.0:8443", got.Bind)
	})
}

// Scenario: cert management is served over the REMOTE (mTLS + passphrase)
// listener too — the operator chose one auth tier (a remote session is already a
// full control-plane operator). With a valid remote session /api/remote-access/
// info returns 200; without a session it is refused at the middleware boundary.
func TestRemoteAccessAdmin_ServedOverRemoteListener(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		_ = newFlow(t)
		setupTestMaterial(t)
		m, err := remoteaccess.Load()
		require.NoError(t, err, "remoteaccess.Load")

		handler := agentd.BuildRemoteDashboardHandlerForTest(m)
		session := &http.Cookie{
			Name:  remoteSessionCookieName,
			Value: remoteaccess.SignCookie(m.CookieKey(), "human", time.Hour),
		}

		withSession := httptest.NewRequest(http.MethodGet, "/api/remote-access/info", nil)
		withSession.AddCookie(session)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, withSession)
		require.Equal(t, http.StatusOK, rec.Code, "info over remote listener body=%s", rec.Body.String())
		var got raInfo
		testharness.DecodeJSON(t, rec, &got)
		assert.True(t, got.MaterialExists)

		noSession := httptest.NewRequest(http.MethodGet, "/api/remote-access/info", nil)
		recNo := httptest.NewRecorder()
		handler.ServeHTTP(recNo, noSession)
		assert.NotEqual(t, http.StatusOK, recNo.Code, "without a remote session the endpoint is refused")
	})
}

// Scenario: a device's .p12 (its private key) is downloadable over the REMOTE
// listener too — the operator's deliberate "serve everything over both" call
// (JOH-278). Pins that decision so a future change can't silently flip it.
func TestRemoteAccessAdmin_ClientP12OverRemoteListener(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		_ = newFlow(t)
		setupTestMaterial(t) // issues "phone"
		m, err := remoteaccess.Load()
		require.NoError(t, err, "remoteaccess.Load")

		handler := agentd.BuildRemoteDashboardHandlerForTest(m)
		req := httptest.NewRequest(http.MethodGet, "/api/remote-access/client?name=phone", nil)
		req.AddCookie(&http.Cookie{
			Name:  remoteSessionCookieName,
			Value: remoteaccess.SignCookie(m.CookieKey(), "human", time.Hour),
		})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, ".p12 over remote listener body=%s", rec.Body.String())
		assert.Equal(t, "application/x-pkcs12", rec.Header().Get("Content-Type"))
		assert.NotEmpty(t, rec.Body.Bytes(), ".p12 download is non-empty over remote")
	})
}

// Scenario: an operator-entered host list is validated before it reaches the
// cert — a junk token is rejected with a 400, not written as a bogus SAN.
func TestRemoteAccessAdmin_AddHostsRejectsInvalid(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		_ = newFlow(t)
		setupTestMaterial(t)
		mux := agentd.BuildDashboardHandlerForTest()

		rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/remote-access/add-hosts",
			map[string]string{"hosts": "good.example.com, bad!!host"}))
		require.Equal(t, http.StatusBadRequest, rec.Code, "an invalid host must be rejected")
		assert.Contains(t, rec.Body.String(), "bad!!host", "the error names the offending token")

		// The valid host was NOT applied (all-or-nothing) — SANs unchanged.
		info := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/remote-access/info", nil))
		var got raInfo
		testharness.DecodeJSON(t, info, &got)
		assert.NotContains(t, got.SANs, "good.example.com", "a rejected batch applies nothing")
	})
}
