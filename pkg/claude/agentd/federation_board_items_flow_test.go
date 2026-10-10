package agentd_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardBoardItemPullPreviewImportWithoutPairing(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	t.Cleanup(agentd.ResetBoardUpdatesForTest(filepath.Join(t.TempDir(), "board-update-notes.json")))
	dash := agentd.BuildDashboardHandlerForTest()
	call := func(method, tail string, p any) *httptest.ResponseRecorder {
		t.Helper()
		return testharness.Serve(dash, testharness.JSONRequest(t, method, "/api/federation/boards"+tail, p))
	}
	must := func(method, tail string, p any) map[string]any {
		t.Helper()
		r := call(method, tail, p)
		require.Equal(t, 200, r.Code, r.Body.String())
		var out map[string]any
		testharness.DecodeJSON(t, r, &out)
		return out
	}
	board := must("POST", "", map[string]any{"name": "library"})["id"].(string)
	invitation := must("POST", "/"+board+"/invites", map[string]any{"role": "publisher", "ttl_seconds": 120})
	token := invitation["token"].(string)
	var invite struct{ Hub, Board, Secret, Key string }
	encoded, e := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "board1_"))
	require.NoError(t, e)
	require.NoError(t, json.Unmarshal(encoded, &invite))
	publisher, e := proto.NewIdentity()
	require.NoError(t, e)
	opts := client.Options{URL: fh.url, Identity: publisher}
	result, e := client.BoardCall(context.Background(), opts, invite.Secret, "keys.join", map[string]any{"board": board, "token": invite.Secret})
	require.NoError(t, e)
	require.Equal(t, 200, result.Status)
	var packageReply struct {
		Epoch   int64  `json:"epoch"`
		Package string `json:"key_package"`
	}
	require.NoError(t, json.Unmarshal(result.Body, &packageReply))
	wrap, e := hex.DecodeString(invite.Key)
	require.NoError(t, e)
	cipher, e := base64.RawStdEncoding.DecodeString(packageReply.Package)
	require.NoError(t, e)
	key, e := proto.OpenBoardContent(wrap, board, 1, invite.Secret, cipher)
	require.NoError(t, e)
	_, e = db.CreateRole(&db.Role{Name: "board-role", Brief: "Review carefully."})
	require.NoError(t, e)
	exported := fedHuman(t, fh.f, "GET", "/v1/config-bundle/export?only=roles/board-role", nil)
	require.Equal(t, 200, exported.Code, exported.Body.String())
	raw := exported.Body.Bytes()
	require.Equal(t, 204, fedHuman(t, fh.f, "DELETE", "/v1/roles/board-role", nil).Code)
	publish := func(payload []byte, name string) proto.BoardItemVersion {
		t.Helper()
		sum := sha256.Sum256(payload)
		m := proto.BoardItemManifest{Item: proto.NewEnvelopeID(), Version: proto.NewEnvelopeID(), Kind: "config", Name: name, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(payload))}
		m.Sign(publisher)
		v := proto.BoardItemVersion{Board: board, Item: proto.NewEnvelopeID(), Version: proto.NewEnvelopeID(), Blob: proto.NewEnvelopeID(), Epoch: 1}
		cipher, e := proto.SealBoardContent(key, board, 1, v.Blob, payload)
		require.NoError(t, e)
		sum = sha256.Sum256(cipher)
		v.SHA256 = hex.EncodeToString(sum[:])
		v.Bytes = int64(len(cipher))
		meta, e := json.Marshal(m)
		require.NoError(t, e)
		v.Metadata, e = proto.SealBoardContent(key, board, 1, v.Version, meta)
		require.NoError(t, e)
		if name == "invalid metadata" {
			v.Metadata[len(v.Metadata)-1] ^= 0xff
		}
		v.Sign(publisher)
		_, e = client.BoardBlob(context.Background(), opts, v, cipher)
		require.NoError(t, e)
		r, e := client.BoardCall(context.Background(), opts, "", "items.publish", map[string]any{"board": board, "version": v})
		require.NoError(t, e)
		require.Equal(t, 200, r.Status, r.Error)
		return v
	}
	v := publish(raw, "review pack")
	base := "/" + board + "/items/" + v.Item + "/versions/" + v.Version
	row, e := fh.store.Get(publisher.ID())
	require.NoError(t, e)
	require.Nil(t, row, "publisher is not fleet admitted")
	peer, e := db.GetFederationPeer(publisher.ID())
	require.NoError(t, e)
	require.Nil(t, peer, "board publication needs no machine trust")
	catalog := must("GET", "/"+board+"/items", nil)
	require.Len(t, catalog["items"], 1)
	require.Equal(t, 409, call("GET", base+"/contents", nil).Code)
	must("POST", base+"/fetch", map[string]any{})
	must("GET", base+"/contents", nil)
	require.Equal(t, 404, call("GET", base+"/contents?path=../../etc/passwd", nil).Code)
	content := must("GET", base+"/contents?path=bundle.json&max_bytes=32", nil)
	require.True(t, content["truncated"].(bool))
	head := call("HEAD", base+"/download", nil)
	require.Equal(t, 200, head.Code)
	require.Empty(t, head.Body.String())
	require.Equal(t, "nosniff", head.Header().Get("X-Content-Type-Options"))
	require.Contains(t, head.Header().Get("Content-Security-Policy"), "sandbox")
	download := call("GET", base+"/download", nil)
	require.Equal(t, 200, download.Code)
	require.Equal(t, string(raw), download.Body.String())
	role, e := db.GetRole("board-role")
	require.NoError(t, e)
	require.Nil(t, role, "fetch never imports")
	preview := must("POST", base+"/preview", map[string]any{})
	previewToken := preview["preview_token"].(string)
	require.Equal(t, 409, call("POST", base+"/import", map[string]any{"preview_token": previewToken, "replace": true}).Code, "options cannot widen after preview")
	preview = must("POST", base+"/preview", map[string]any{})
	previewToken = preview["preview_token"].(string)
	_, e = db.CreateRole(&db.Role{Name: "board-role", Brief: "A newer local choice."})
	require.NoError(t, e)
	stale := call("POST", base+"/import", map[string]any{"preview_token": previewToken})
	require.Equal(t, 409, stale.Code, stale.Body.String())
	require.Contains(t, stale.Body.String(), "preview_changed")
	_, e = db.DeleteRole("board-role")
	require.NoError(t, e)
	preview = must("POST", base+"/preview", map[string]any{})
	previewToken = preview["preview_token"].(string)
	must("POST", base+"/import", map[string]any{"preview_token": previewToken})
	role, e = db.GetRole("board-role")
	require.NoError(t, e)
	require.Equal(t, "Review carefully.", role.Brief)
	require.Equal(t, 409, call("POST", base+"/import", map[string]any{"preview_token": previewToken}).Code, "single-use import token")
	must("PUT", "/"+board+"/items/"+v.Item+"/pin", map[string]any{"version": v.Version})
	copiedBoard := must("POST", "", map[string]any{"name": "copy"})["id"].(string)
	copyReceipt := must("POST", "/"+copiedBoard+"/items", map[string]any{"source": map[string]string{"board": board, "item": v.Item, "version": v.Version}})
	require.NotEqual(t, publisher.ID(), copyReceipt["publisher"])
	copied := must("GET", "/"+copiedBoard+"/items", nil)["items"].([]any)[0].(map[string]any)
	require.Equal(t, publisher.ID(), copied["publisher"], "original provenance survives re-publication")
	copiedItem := copyReceipt["item"].(string)
	copiedVersion := copyReceipt["version"].(string)
	must("PUT", "/"+copiedBoard+"/items/"+copiedItem+"/pin", map[string]any{"version": copiedVersion})
	update := must("POST", "/"+copiedBoard+"/items", map[string]any{"name": "receiver edition", "item": copiedItem, "parent": copiedVersion, "only": []string{"roles/board-role"}})
	require.NotEqual(t, copiedVersion, update["version"])
	stalePublish := call("POST", "/"+copiedBoard+"/items", map[string]any{"name": "stale edition", "item": copiedItem, "parent": copiedVersion, "only": []string{"roles/board-role"}})
	require.Equal(t, 409, stalePublish.Code, stalePublish.Body.String())
	updated := must("GET", "/"+copiedBoard+"/items", nil)["items"].([]any)[0].(map[string]any)
	require.Equal(t, update["version"], updated["latest_version"])
	require.Equal(t, copiedVersion, updated["pinned_version"])
	require.Equal(t, true, updated["update_available"])

	// tcl-1gpqb1: the kept item with a newer version is listed for the badge;
	// Messages notes are opt-in and announce only versions first seen later.
	updates := must("GET", "/updates?refresh=1", nil)
	require.Equal(t, false, updates["notify"])
	require.Len(t, updates["updates"], 1)
	notice := updates["updates"].([]any)[0].(map[string]any)
	require.Equal(t, copiedBoard, notice["board"])
	require.Equal(t, "copy", notice["board_name"])
	require.Equal(t, "receiver edition", notice["name"])
	require.Equal(t, copiedVersion, notice["pinned_version"])
	require.Equal(t, update["version"], notice["latest_version"])
	require.Equal(t, 400, call("PUT", "/updates", map[string]any{"notify": "yes"}).Code)
	require.Equal(t, true, must("PUT", "/updates", map[string]any{"notify": true})["notify"])
	third := must("POST", "/"+copiedBoard+"/items", map[string]any{"name": "third edition", "item": copiedItem, "parent": update["version"], "only": []string{"roles/board-role"}})
	agentd.ExpireBoardUpdatesForTest()
	updates = must("GET", "/updates", nil)
	require.Equal(t, true, updates["notify"])
	require.Equal(t, third["version"], updates["updates"].([]any)[0].(map[string]any)["latest_version"])
	notes, e := db.ListHumanMessages()
	require.NoError(t, e)
	announced := 0
	for _, n := range notes {
		if strings.HasPrefix(n.Subject, "Board update: ") {
			announced++
			require.Equal(t, "Board update: third edition", n.Subject, "the version known before opting in is not announced")
			require.Contains(t, n.Body, "nothing is fetched or imported")
		}
	}
	require.Equal(t, 1, announced)
	agentd.ExpireBoardUpdatesForTest()
	must("GET", "/updates", nil)
	notes, e = db.ListHumanMessages()
	require.NoError(t, e)
	again := 0
	for _, n := range notes {
		if strings.HasPrefix(n.Subject, "Board update: ") {
			again++
		}
	}
	require.Equal(t, 1, again, "one note per version")
	// A version posted while the daemon is down is announced once it is back.
	agentd.RestartBoardUpdatesForTest()
	fourth := must("POST", "/"+copiedBoard+"/items", map[string]any{"name": "fourth edition", "item": copiedItem, "parent": third["version"], "only": []string{"roles/board-role"}})
	must("GET", "/updates", nil)
	notes, e = db.ListHumanMessages()
	require.NoError(t, e)
	subjects := []string{}
	for _, n := range notes {
		if strings.HasPrefix(n.Subject, "Board update: ") {
			subjects = append(subjects, n.Subject)
		}
	}
	require.ElementsMatch(t, []string{"Board update: third edition", "Board update: fourth edition"}, subjects)
	// Keeping the latest version clears it at once, not after the cache ages.
	must("PUT", "/"+copiedBoard+"/items/"+copiedItem+"/pin", map[string]any{"version": fourth["version"]})
	require.Empty(t, must("GET", "/updates", nil)["updates"])

	var poisoned configbundle.Bundle
	require.NoError(t, json.Unmarshal(raw, &poisoned))
	poisoned.Sections["roles"][0].Value = json.RawMessage(`{"name":"board-role","brief":"api_key=supersecret123456789"}`)
	unsafe, e := json.Marshal(poisoned)
	require.NoError(t, e)
	bad := publish(unsafe, "unsafe pack")
	rejected := call("POST", "/"+board+"/items/"+bad.Item+"/versions/"+bad.Version+"/fetch", map[string]any{})
	require.NotEqual(t, 200, rejected.Code)
	require.Contains(t, rejected.Body.String(), "credentials")
	invalid := publish(raw, "invalid metadata")
	page := must("GET", "/"+board+"/items", nil)
	require.Contains(t, page, "next_cursor")
	foundInvalid, foundValid := false, false
	for _, entry := range page["items"].([]any) {
		row := entry.(map[string]any)
		if row["id"] == invalid.Item {
			require.Equal(t, true, row["invalid"])
			foundInvalid = true
		}
		if row["id"] == v.Item {
			foundValid = true
		}
	}
	require.True(t, foundInvalid)
	require.True(t, foundValid, "invalid publisher metadata must not hide valid neighbors")
	history := must("GET", "/"+board+"/items/"+invalid.Item+"/versions", nil)
	require.Equal(t, true, history["versions"].([]any)[0].(map[string]any)["invalid"])
	require.NotEqual(t, 200, call("POST", "/"+board+"/items/"+invalid.Item+"/versions/"+invalid.Version+"/fetch", map[string]any{}).Code)
	for _, op := range []struct{ method, tail string }{{"GET", "/updates"}, {"PUT", "/updates"}, {"GET", "/" + board + "/items"}, {"POST", "/" + board + "/items"}, {"POST", base + "/import"}, {"GET", base + "/download"}} {
		require.Equal(t, 403, testharness.Serve(agentd.PeerViewHandler(fh.peer.id.ID()), testharness.JSONRequest(t, op.method, "/api/federation/boards"+op.tail, map[string]any{})).Code)
	}
	require.Equal(t, 403, testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, "GET", "/v1/federation/boards/"+board+"/items", nil), "agent")).Code)
}
