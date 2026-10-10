package hub_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func TestBoardItemsCiphertextStreamPublicationPinAndAuthority(t *testing.T) {
	_, live, url := newHub(t, hub.Config{})
	owner, e := proto.NewIdentity()
	require.NoError(t, e)
	require.NoError(t, live.Admit(owner.ID()))
	ws, ch := boardSocket(t, url, owner, "")
	board := proto.NewEnvelopeID()
	key := make([]byte, 32)
	box, e := proto.SealBoardKey(owner.Pub, board, 1, key)
	require.NoError(t, e)
	r := boardCall(t, ws, owner, ch, "boards.create", map[string]any{"board": board, "name": "catalog", "key_proofs": map[string][]byte{owner.ID(): proto.BoardKeyProof(key, board, 1, owner.ID())}, "envelopes": map[string]*proto.Encrypted{owner.ID(): box}})
	require.Equal(t, 200, r.Status, r.Error)
	blob := proto.NewEnvelopeID()
	cipher, e := proto.SealBoardContent(key, board, 1, blob, []byte("private config payload"))
	require.NoError(t, e)
	sum := sha256.Sum256(cipher)
	v := proto.BoardItemVersion{Board: board, Item: proto.NewEnvelopeID(), Version: proto.NewEnvelopeID(), Blob: blob, Epoch: 1, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(cipher)), Metadata: []byte("encrypted metadata")}
	v.Sign(owner)
	opts := client.Options{URL: url, Identity: owner}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, e = client.BoardBlob(ctx, opts, v, cipher)
	require.NoError(t, e)
	r = boardCall(t, ws, owner, ch, "items.publish", map[string]any{"board": board, "version": v})
	require.Equal(t, 200, r.Status, r.Error)
	r = boardCall(t, ws, owner, ch, "items.list", map[string]any{"board": board})
	require.Equal(t, 200, r.Status, r.Error)
	var catalog struct {
		Items []proto.BoardItemVersion `json:"items"`
	}
	require.NoError(t, json.Unmarshal(r.Body, &catalog))
	require.Len(t, catalog.Items, 1)
	require.Equal(t, v.Version, catalog.Items[0].Version)
	fetched, e := client.BoardBlob(ctx, opts, v, nil)
	require.NoError(t, e)
	require.Equal(t, cipher, fetched)
	r = boardCall(t, ws, owner, ch, "pins.set", map[string]any{"board": board, "item": v.Item, "version_id": v.Version})
	require.Equal(t, 200, r.Status, r.Error)
	r = boardCall(t, ws, owner, ch, "items.publish", map[string]any{"board": board, "version": v})
	require.Equal(t, 409, r.Status, "immutable version")
	reader, e := proto.NewIdentity()
	require.NoError(t, e)
	token := proto.NewEnvelopeID()
	r = boardCall(t, ws, owner, ch, "invites.create", map[string]any{"board": board, "epoch": 1, "token": token, "ttl_seconds": 120, "role": "reader", "key_package": "encrypted"})
	require.Equal(t, 200, r.Status, r.Error)
	readerWS, readerChallenge := boardSocket(t, url, reader, token)
	fetched, e = client.BoardBlob(ctx, client.Options{URL: url, Identity: reader}, v, nil)
	require.NoError(t, e)
	require.Equal(t, cipher, fetched)
	next := v
	next.Version = proto.NewEnvelopeID()
	next.Parent = v.Version
	next.Sign(reader)
	r = boardCall(t, readerWS, reader, readerChallenge, "items.publish", map[string]any{"board": board, "version": next})
	require.Equal(t, 403, r.Status)
	_, e = client.BoardBlob(ctx, client.Options{URL: url, Identity: reader}, next, cipher)
	require.Error(t, e, "reader cannot upload")
	r = boardCall(t, ws, owner, ch, "members.remove", map[string]any{"board": board, "instance": reader.ID()})
	require.Equal(t, 200, r.Status)
	_, e = client.BoardBlob(ctx, client.Options{URL: url, Identity: reader}, v, nil)
	require.Error(t, e, "removed board-only member loses streams")
}
