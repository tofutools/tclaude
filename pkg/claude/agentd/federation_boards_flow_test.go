package agentd_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardContentBoardMembershipKeysAndModeration(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	dash := agentd.BuildDashboardHandlerForTest()
	call := func(method, tail string, p any) *httptest.ResponseRecorder {
		t.Helper()
		return testharness.Serve(dash, testharness.JSONRequest(t, method, "/api/federation/boards"+tail, p))
	}
	must := func(method, tail string, p any) map[string]any {
		t.Helper()
		rec := call(method, tail, p)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var body map[string]any
		testharness.DecodeJSON(t, rec, &body)
		return body
	}
	created := must("POST", "", map[string]any{"name": "recipes"})
	board := created["id"].(string)
	require.Equal(t, "owner", created["role"])
	require.NotEmpty(t, must("GET", "", nil)["boards"])
	v1 := fedHuman(t, fh.f, "GET", "/v1/federation/boards/"+board, nil)
	require.Equal(t, 200, v1.Code, v1.Body.String())
	require.Equal(t, 409, call("DELETE", "/"+board+"/membership", nil).Code)
	invitation := must("POST", "/"+board+"/invites", map[string]any{"role": "reader", "ttl_seconds": 120})
	token := invitation["token"].(string)
	require.NotEmpty(t, token)
	require.NotEmpty(t, invitation["token_id"])
	var decoded struct{ Hub, Board, Secret, Key string }
	raw, err := base64.RawURLEncoding.DecodeString(token[len("board1_"):])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &decoded))
	reader, err := proto.NewIdentity()
	require.NoError(t, err)
	opts := client.Options{URL: fh.url, Identity: reader}
	result, err := client.BoardCall(context.Background(), opts, decoded.Secret, "keys.join", map[string]any{"board": board, "token": decoded.Secret})
	require.NoError(t, err)
	require.Equal(t, 200, result.Status, result.Error)
	var pack struct {
		Epoch   int64  `json:"epoch"`
		Package string `json:"key_package"`
	}
	require.NoError(t, json.Unmarshal(result.Body, &pack))
	wrap, err := hex.DecodeString(decoded.Key)
	require.NoError(t, err)
	cipher, err := base64.RawStdEncoding.DecodeString(pack.Package)
	require.NoError(t, err)
	key, err := proto.OpenBoardContent(wrap, board, pack.Epoch, decoded.Secret, cipher)
	require.NoError(t, err)
	require.Len(t, key, 32)
	envelope, err := proto.SealBoardKey(reader.Pub, board, pack.Epoch, key)
	require.NoError(t, err)
	result, err = client.BoardCall(context.Background(), opts, "", "keys.install", map[string]any{"board": board, "epoch": pack.Epoch, "envelopes": map[string]*proto.Encrypted{reader.ID(): envelope}, "key_proofs": map[string][]byte{reader.ID(): proto.BoardKeyProof(key, board, pack.Epoch, reader.ID())}})
	require.NoError(t, err)
	require.Equal(t, 200, result.Status, result.Error)
	row, err := fh.store.Get(reader.ID())
	require.NoError(t, err)
	require.Nil(t, row, "board member is not fleet admitted")
	members := must("GET", "/"+board+"/members", nil)
	require.Len(t, members["members"], 2)
	result, err = client.BoardCall(context.Background(), opts, "", "invites.create", map[string]any{"board": board, "role": "publisher", "token": proto.NewEnvelopeID(), "ttl_seconds": 120, "epoch": 1, "key_package": "opaque"})
	require.NoError(t, err)
	require.Equal(t, 403, result.Status)
	must("PUT", "/"+board+"/members/"+reader.ID(), map[string]any{"role": "publisher"})
	require.Equal(t, float64(2), must("POST", "/"+board+"/rotate-key", map[string]any{})["epoch"])
	result, err = client.BoardCall(context.Background(), opts, "", "keys.get", map[string]any{"board": board})
	require.NoError(t, err)
	require.Equal(t, 200, result.Status)
	var keys struct {
		Keys map[string]*proto.Encrypted `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(result.Body, &keys))
	newKey, err := proto.OpenBoardKey(reader, board, 2, keys.Keys["2"])
	require.NoError(t, err)
	require.NotEqual(t, key, newKey)
	must("DELETE", "/"+board+"/members/"+reader.ID(), nil)
	_, err = client.BoardCall(context.Background(), opts, "", "boards.list", map[string]any{})
	require.Error(t, err, "removed board-only member loses connection admission")
	must("DELETE", "/"+board+"/invites/"+invitation["token_id"].(string), nil)
	// The receiver joins another board through its human API, without adding a
	// peer trust/grant path. Only opaque invitation material carries the key.
	other := proto.NewEnvelopeID()
	otherKey := make([]byte, 32)
	_, err = rand.Read(otherKey)
	require.NoError(t, err)
	box, err := proto.SealBoardKey(fh.peer.id.Pub, other, 1, otherKey)
	require.NoError(t, err)
	peerOpts := client.Options{URL: fh.url, Identity: fh.peer.id}
	result, err = client.BoardCall(context.Background(), peerOpts, "", "boards.create", map[string]any{"board": other, "name": "other", "envelopes": map[string]*proto.Encrypted{fh.peer.id.ID(): box}, "key_proofs": map[string][]byte{fh.peer.id.ID(): proto.BoardKeyProof(otherKey, other, 1, fh.peer.id.ID())}})
	require.NoError(t, err)
	require.Equal(t, 200, result.Status, result.Error)
	bearer := proto.NewEnvelopeID()
	wrapping := make([]byte, 32)
	_, err = rand.Read(wrapping)
	require.NoError(t, err)
	cipher, err = proto.SealBoardContent(wrapping, other, 1, bearer, otherKey)
	require.NoError(t, err)
	result, err = client.BoardCall(context.Background(), peerOpts, "", "invites.create", map[string]any{"board": other, "token": bearer, "role": "reader", "ttl_seconds": 120, "epoch": 1, "key_package": base64.RawStdEncoding.EncodeToString(cipher)})
	require.NoError(t, err)
	require.Equal(t, 200, result.Status, result.Error)
	raw, err = json.Marshal(map[string]string{"hub": fh.url, "board": other, "secret": bearer, "key": hex.EncodeToString(wrapping)})
	require.NoError(t, err)
	joined := must("POST", "/join", map[string]any{"token": "board1_" + base64.RawURLEncoding.EncodeToString(raw)})
	require.Equal(t, "reader", joined["role"])
	must("DELETE", "/"+other+"/membership", nil)
	// A used invite is refused by the hub in the board hello: the UI gets the
	// invite code, not a generic upstream failure.
	again := call("POST", "/join", map[string]any{"token": "board1_" + base64.RawURLEncoding.EncodeToString(raw)})
	require.Equal(t, 403, again.Code, again.Body.String())
	require.Contains(t, again.Body.String(), `"board_invite"`)
	// Even an unrestricted peer cannot use local board authority.
	peerView := agentd.PeerViewHandler(fh.peer.id.ID())
	for _, endpoint := range []struct{ method, tail string }{
		{"GET", ""}, {"POST", ""}, {"POST", "/join"}, {"GET", "/" + board},
		{"GET", "/" + board + "/members"}, {"PUT", "/" + board + "/members/" + reader.ID()},
		{"DELETE", "/" + board + "/members/" + reader.ID()}, {"DELETE", "/" + board + "/membership"},
		{"POST", "/" + board + "/invites"}, {"DELETE", "/" + board + "/invites/token"},
		{"POST", "/" + board + "/rotate-key"},
	} {
		rec := testharness.Serve(peerView, testharness.JSONRequest(t, endpoint.method, "/api/federation/boards"+endpoint.tail, map[string]any{}))
		require.Equal(t, 403, rec.Code, endpoint.method+" "+endpoint.tail)
	}

	// Human-only routes must refuse agent authority, including read surfaces.
	for _, method := range []string{"GET", "POST"} {
		req := agentd.AsAgentPeer(testharness.JSONRequest(t, method, "/v1/federation/boards", map[string]any{}), "board-agent")
		rec := testharness.Serve(fh.f.Mux, req)
		require.Equal(t, 403, rec.Code)
	}
}
