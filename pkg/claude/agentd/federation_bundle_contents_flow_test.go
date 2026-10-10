package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/config"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardFederationBundleContentsAndDownload(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	fedGrantConfig(t, fh)
	fh.f.HaveGroup("receiver")
	fedReceiveAgents(t, fh, "receiver")
	h := agentd.BuildDashboardHandlerForTest()
	call := func(method, path string, status int) *httptest.ResponseRecorder {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, path, nil))
		require.Equal(t, status, rec.Code, rec.Body.String())
		require.Equal(t, "sandbox", rec.Header().Get("Content-Security-Policy"))
		require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
		return rec
	}
	configRaw := fedOfferedConfig(t, "<script>Never render as HTML</script>")
	config := fedSendOffer(t, fh.peer, configRaw)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, config.ID).Status)
	b := fedAgentBundle(t)
	transcript := []byte(strings.Repeat("{\"text\":\"hello\"}\n", 20000) + string([]byte{0xff}))
	b.SetHistory("jsonl", "source-conversation", transcript)
	raw, err := b.Encode()
	require.NoError(t, err)
	agent := fedAgentOffer(t, fh.peer, "receiver", b)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, agent.ID).Status)
	for _, item := range []struct {
		d     bundletransfer.Descriptor
		raw   []byte
		paths []string
	}{{config, configRaw, []string{"bundle.json", "sections/profiles.json", "sections/roles.json"}}, {agent, raw, []string{agentbundle.HistoryFile, agentbundle.ManifestFile}}} {
		base := "/api/federation/bundle-offers/" + item.d.ID
		suffix := "?peer=" + fh.peer.id.ID()
		rec := call("GET", base+"/contents"+suffix, 200)
		var listing struct {
			Type    string `json:"type"`
			Entries []struct {
				Path string `json:"path"`
				Size int64  `json:"size"`
				Kind string `json:"kind"`
			} `json:"entries"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listing))
		require.Equal(t, item.d.Type, listing.Type)
		paths := []string{}
		for _, e := range listing.Entries {
			paths = append(paths, e.Path)
			require.Positive(t, e.Size)
		}
		require.Equal(t, item.paths, paths)
		rec = call("GET", base+"/download"+suffix, 200)
		require.Equal(t, item.raw, rec.Body.Bytes())
		require.Equal(t, "application/octet-stream", rec.Header().Get("Content-Type"))
		require.Contains(t, rec.Header().Get("Content-Disposition"), "attachment; filename=\"tclaude-")
		rec = call("HEAD", base+"/download"+suffix, 200)
		require.Empty(t, rec.Body.Bytes())
		require.NotEmpty(t, rec.Header().Get("Content-Length"))
		call("GET", base+"/contents"+suffix+"&path=../../db.sqlite", 404)
		for _, tail := range []string{"contents", "download"} {
			endpoint := base + "/" + tail + suffix
			rec = testharness.Serve(agentd.PeerViewHandler(fh.peer.id.ID()), testharness.JSONRequest(t, "GET", endpoint, nil))
			require.Equal(t, 403, rec.Code, rec.Body.String())
			rec = testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "GET", strings.Replace(endpoint, "/api/", "/v1/", 1), nil), "agent-caller"))
			require.Equal(t, 403, rec.Code, rec.Body.String())
			mux := http.NewServeMux()
			agentd.RegisterDashboardRoutesForTest(mux)
			rec = testharness.Serve(mux, testharness.JSONRequest(t, "GET", endpoint, nil))
			require.Equal(t, 403, rec.Code, rec.Body.String())
		}
	}
	base := "/api/federation/bundle-offers/" + agent.ID + "/contents?peer=" + fh.peer.id.ID() + "&path=" + url.QueryEscape(agentbundle.HistoryFile)
	var page struct {
		Text      string `json:"text"`
		Truncated bool   `json:"truncated"`
		Offset    int64  `json:"offset"`
		Next      int64  `json:"next_offset"`
	}
	rec := call("GET", base+"&max_bytes=16", 200)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.True(t, page.Truncated)
	require.Equal(t, int64(16), page.Next)
	require.Equal(t, string(transcript[:16]), page.Text)
	rec = call("GET", base+"&max_bytes=16&offset=16", 200)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Equal(t, int64(16), page.Offset)
	require.Equal(t, string(transcript[16:32]), page.Text)
	rec = call("GET", base+"&offset="+jsonNumber(len(transcript)-1), 200)
	require.Contains(t, rec.Body.String(), "�")
	call("GET", base+"&max_bytes=1048577", 400)
	call("GET", base+"&offset=-1", 400)
	rec = call("GET", "/api/federation/bundle-offers/"+config.ID+"/contents?path=sections/roles.json", 200)
	require.Contains(t, rec.Body.String(), "Never render as HTML")
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	// A real revoke removes inspection authority even though the spool is ready.
	rec = fedHuman(t, fh.f, "DELETE", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": "config.offer"})
	require.Equal(t, 200, rec.Code)
	call("GET", "/api/federation/bundle-offers/"+config.ID+"/download", 403)
}

func jsonNumber(n int) string { raw, _ := json.Marshal(n); return string(raw) }

func TestDashboardFederationBundleContentsUnavailableStates(t *testing.T) {
	fh := newFedHarness(t)
	fedGrantConfig(t, fh)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	h := agentd.BuildDashboardHandlerForTest()
	for _, state := range []string{"pending", "declined", "expired"} {
		raw := fedOfferedConfig(t, state)
		d := bundletransfer.New(bundletransfer.Config, raw, "test", time.Now().Add(time.Hour))
		if state == "expired" {
			d.ExpiresAt = time.Now().Add(-time.Hour)
		}
		_, err := db.InsertFederationBundleOffer(db.FederationBundleOffer{Descriptor: d, Direction: "in", Peer: fh.peer.id.ID(), State: state}, bundletransfer.Config)
		require.NoError(t, err)
		status := 410
		if state == "pending" {
			status = 409
		}
		for _, tail := range []string{"contents", "download"} {
			rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/federation/bundle-offers/"+d.ID+"/"+tail, nil))
			require.Equal(t, status, rec.Code, rec.Body.String())
		}
	}
	// Ready metadata alone cannot turn modified spool bytes into a download.
	raw := fedOfferedConfig(t, "verified")
	d := fedSendOffer(t, fh.peer, raw)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, d.ID).Status)
	// Replace through the bounded spool writer using the same key but new digest.
	changed := bytes.ReplaceAll(raw, []byte("verified"), []byte("tampered"))
	other := bundletransfer.New(bundletransfer.Config, changed, "test", d.ExpiresAt)
	other.ID = d.ID
	spool := bundletransfer.Spool{Root: config.DataDir() + "/federation/bundles"}
	require.NoError(t, spool.Receive("in", fh.peer.id.ID(), other, bytes.NewReader(changed)))
	rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/federation/bundle-offers/"+d.ID+"/download", nil))
	require.Equal(t, 409, rec.Code, rec.Body.String())
}
