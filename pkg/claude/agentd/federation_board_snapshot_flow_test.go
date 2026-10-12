package agentd_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// A snapshot shared on a board reaches only its named recipients, lands
// as a new agent through preview and import, and is gone once deleted.
func TestBoardSnapshotShareFetchImportDelete(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	f.HaveGroup("receiver")
	const source = "019fe740-43a4-7023-b8ae-1ee64459f2e1"
	f.HaveAliveSession(source, "original", "original-pane", testutil.CanonicalTempDir(t))
	human := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		return fedHuman(t, f, method, path, body)
	}
	decode := func(rec *httptest.ResponseRecorder, out any) {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), out))
	}

	var created struct {
		ID string `json:"id"`
	}
	decode(human("POST", "/v1/federation/boards", map[string]any{"name": "team-board"}), &created)
	board := created.ID
	base := "/v1/federation/boards/" + board + "/snapshots"

	// Another instance joins the board as a publisher.
	var invitation struct {
		Token string `json:"token"`
	}
	decode(human("POST", "/v1/federation/boards/"+board+"/invites", map[string]any{"role": "publisher", "ttl_seconds": 120}), &invitation)
	var invite struct{ Hub, Board, Secret, Key string }
	encoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(invitation.Token, "board1_"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, &invite))
	member, err := proto.NewIdentity()
	require.NoError(t, err)
	memberOpts := client.Options{URL: fh.url, Identity: member}
	joined, err := client.BoardCall(context.Background(), memberOpts, invite.Secret, "keys.join", map[string]any{"board": board, "token": invite.Secret})
	require.NoError(t, err)
	require.Equal(t, 200, joined.Status, string(joined.Body))
	var pkg struct {
		Package string `json:"key_package"`
	}
	require.NoError(t, json.Unmarshal(joined.Body, &pkg))
	wrap, err := hex.DecodeString(invite.Key)
	require.NoError(t, err)
	sealedKey, err := base64.RawStdEncoding.DecodeString(pkg.Package)
	require.NoError(t, err)
	boardKey, err := proto.OpenBoardContent(wrap, board, 1, invite.Secret, sealedKey)
	require.NoError(t, err)

	decode(human("POST", "/v1/snapshots", map[string]any{"agent": source, "name": "shared"}), &struct{}{})

	share := func(q url.Values) *httptest.ResponseRecorder {
		t.Helper()
		return testharness.Serve(f.Mux, agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, base+"?"+q.Encode(), nil)))
	}
	q := url.Values{"snapshot": {"shared"}, "to": {member.ID()}, "expires": {"3d"}}
	rec := share(url.Values{"snapshot": {"shared"}, "to": {"inst_not_a_member"}})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = share(url.Values{"snapshot": {"shared"}, "to": {member.ID()}, "expires": {"31d"}})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// Without the reviewed digest nothing is published.
	var preview struct {
		Recipients []string  `json:"recipients"`
		ExpiresAt  time.Time `json:"expires_at"`
		Published  bool      `json:"published"`
		Item       string    `json:"item"`
		Version    string    `json:"version"`
		Summary    struct {
			Digest string `json:"content_digest"`
		} `json:"summary"`
	}
	decode(share(q), &preview)
	assert.False(t, preview.Published)
	self := agentd.FederationInstanceIDForTest()
	assert.ElementsMatch(t, []string{self, member.ID()}, preview.Recipients, "this node is always a recipient")
	assert.WithinDuration(t, time.Now().Add(72*time.Hour), preview.ExpiresAt, time.Minute)
	require.NotEmpty(t, preview.Summary.Digest)
	stale := url.Values{"snapshot": {"shared"}, "to": {member.ID()}, "content_digest": {strings.Repeat("0", 64)}}
	assert.Equal(t, http.StatusConflict, share(stale).Code)

	// A PAF file shared without --name is named after its agent.
	exported := human("GET", "/v1/snapshots/shared/export", nil)
	require.Equal(t, http.StatusOK, exported.Code)
	var fromFile struct {
		Name string `json:"name"`
	}
	decode(testharness.Serve(f.Mux, agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, base+"?to="+member.ID(), bytes.NewReader(exported.Body.Bytes())))), &fromFile)
	assert.NotEmpty(t, fromFile.Name)

	q.Set("content_digest", preview.Summary.Digest)
	decode(share(q), &preview)
	require.True(t, preview.Published)
	ours := preview.Item + "/" + preview.Version

	// The member publishes a snapshot addressed only to itself.
	payload := []byte("sealed for the member only")
	sum := sha256.Sum256(payload)
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	theirs := proto.BoardSnapshotVersion{Board: board, Item: proto.NewEnvelopeID(), Version: proto.NewEnvelopeID(), Epoch: 1, PayloadSHA256: hex.EncodeToString(sum[:]), PayloadBytes: int64(len(payload)), Recipients: []string{member.ID()}, ExpiresAt: expires}
	m := proto.BoardSnapshotManifest{Item: theirs.Item, Version: theirs.Version, Name: "private", Kind: "agent-snapshot", PayloadSHA256: theirs.PayloadSHA256, PayloadBytes: theirs.PayloadBytes, Recipients: theirs.Recipients, ExpiresAt: expires}
	m.Sign(member)
	metadata, err := json.Marshal(m)
	require.NoError(t, err)
	theirs.Metadata, err = proto.SealBoardContent(boardKey, board, 1, theirs.Version, metadata)
	require.NoError(t, err)
	blob := proto.NewEnvelopeID()
	chunk, err := proto.SealBoardContent(boardKey, board, 1, blob, payload)
	require.NoError(t, err)
	chunkSum := sha256.Sum256(chunk)
	theirs.Chunks = []proto.BoardSnapshotChunk{{Blob: blob, SHA256: hex.EncodeToString(chunkSum[:]), Bytes: int64(len(chunk))}}
	theirs.Sign(member)
	_, err = client.BoardSnapshotBlob(context.Background(), memberOpts, theirs, 0, chunk)
	require.NoError(t, err)
	published, err := client.BoardCall(context.Background(), memberOpts, "", "snapshots.publish", map[string]any{"board": board, "snapshot": theirs})
	require.NoError(t, err)
	require.Equal(t, 200, published.Status, string(published.Body))

	var list struct {
		Snapshots []struct {
			Item    string `json:"item"`
			Version string `json:"version"`
			Name    string `json:"name"`
			ForMe   bool   `json:"for_me"`
		} `json:"snapshots"`
	}
	decode(human("GET", base, nil), &list)
	require.Len(t, list.Snapshots, 2)
	forMe := map[string]bool{}
	for _, s := range list.Snapshots {
		forMe[s.Item+"/"+s.Version] = s.ForMe
	}
	assert.True(t, forMe[ours])
	assert.False(t, forMe[theirs.Item+"/"+theirs.Version])

	// A member that is not a recipient gets nothing to import.
	rec = human("POST", base+"/"+theirs.Item+"/"+theirs.Version+"/fetch", map[string]any{})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not_recipient")
	rec = human("POST", base+"/"+theirs.Item+"/"+theirs.Version+"/preview", map[string]any{"group": "receiver", "keep_paths": true})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	// Import needs a fetch, then a preview token for the same options.
	rec = human("POST", base+"/"+ours+"/preview", map[string]any{"group": "receiver", "keep_paths": true})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	decode(human("POST", base+"/"+ours+"/fetch", map[string]any{}), &struct{}{})
	// A recipient can discard its fetched copy without touching the board.
	decode(human("DELETE", base+"/"+ours+"?local=true", nil), &struct{}{})
	rec = human("POST", base+"/"+ours+"/preview", map[string]any{"group": "receiver", "keep_paths": true})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	decode(human("POST", base+"/"+ours+"/fetch", map[string]any{}), &struct{}{})
	var previewed struct {
		Token string `json:"preview_token"`
	}
	decode(human("POST", base+"/"+ours+"/preview", map[string]any{"group": "receiver", "keep_paths": true, "name": "forked"}), &previewed)
	require.NotEmpty(t, previewed.Token)
	rec = human("POST", base+"/"+ours+"/import", map[string]any{"group": "receiver", "keep_paths": true, "name": "other-name", "preview_token": previewed.Token})
	assert.Equal(t, http.StatusConflict, rec.Code, "a token is bound to the previewed options")
	decode(human("POST", base+"/"+ours+"/preview", map[string]any{"group": "receiver", "keep_paths": true, "name": "forked"}), &previewed)
	var imported struct {
		Applied bool                `json:"applied"`
		Spawn   agent.SpawnResponse `json:"spawn"`
	}
	decode(human("POST", base+"/"+ours+"/import", map[string]any{"group": "receiver", "keep_paths": true, "name": "forked", "preview_token": previewed.Token}), &imported)
	require.True(t, imported.Applied)
	assert.NotEqual(t, source, imported.Spawn.ConvID, "the import is a new agent")
	brief := arrivalInbox(t, imported.Spawn.ConvID)
	assert.Contains(t, brief, "board snapshot import")
	assert.Contains(t, brief, "Untrusted history")
	rec = human("POST", base+"/"+ours+"/import", map[string]any{"group": "receiver", "keep_paths": true, "name": "forked", "preview_token": previewed.Token})
	assert.Equal(t, http.StatusConflict, rec.Code, "a token is single-use")

	// Deleting removes it from the board.
	decode(human("DELETE", base+"/"+ours, nil), &struct{}{})
	decode(human("GET", base, nil), &list)
	require.Len(t, list.Snapshots, 1)
	assert.False(t, slices.ContainsFunc(list.Snapshots, func(s struct {
		Item    string `json:"item"`
		Version string `json:"version"`
		Name    string `json:"name"`
		ForMe   bool   `json:"for_me"`
	}) bool {
		return s.Item+"/"+s.Version == ours
	}))
	rec = human("POST", base+"/"+ours+"/fetch", map[string]any{})
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}
