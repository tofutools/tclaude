package hub_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testutil"
)

type snapshotFixture struct {
	store           *hub.Store
	dir, url, board string
	owner           *proto.Identity
	key             []byte
	now             atomic.Int64
}

func newSnapshotFixture(t *testing.T) *snapshotFixture {
	return newSnapshotFixtureWithConfig(t, hub.Config{FramesPerMinute: 10000})
}
func newSnapshotFixtureWithConfig(t *testing.T, cfg hub.Config) *snapshotFixture {
	t.Helper()
	f := &snapshotFixture{dir: testutil.CanonicalTempDir(t), board: proto.NewEnvelopeID(), key: make([]byte, 32)}
	f.now.Store(time.Now().UTC().Truncate(time.Second).UnixNano())
	var e error
	f.store, e = hub.OpenStoreWithClock(filepath.Join(f.dir, "hub.sqlite"), func() time.Time { return time.Unix(0, f.now.Load()).UTC() })
	require.NoError(t, e)
	h, e := hub.New(f.store, cfg)
	require.NoError(t, e)
	srv := httptest.NewServer(h.Handler())
	f.url = "ws" + strings.TrimPrefix(srv.URL, "http")
	t.Cleanup(func() { h.Close(); srv.Close(); require.NoError(t, f.store.Close()) })
	f.owner, e = proto.NewIdentity()
	require.NoError(t, e)
	require.NoError(t, f.store.Admit(f.owner.ID()))
	box, e := proto.SealBoardKey(f.owner.Pub, f.board, 1, f.key)
	require.NoError(t, e)
	r, e := client.BoardCall(context.Background(), client.Options{URL: f.url, Identity: f.owner}, "", "boards.create", map[string]any{"board": f.board, "name": "snapshots", "key_proofs": map[string][]byte{f.owner.ID(): proto.BoardKeyProof(f.key, f.board, 1, f.owner.ID())}, "envelopes": map[string]*proto.Encrypted{f.owner.ID(): box}})
	require.NoError(t, e)
	require.Equal(t, 200, r.Status, r.Error)
	return f
}
func (f *snapshotFixture) call(t *testing.T, id *proto.Identity, method string, p map[string]any) *proto.HubAdminResult {
	t.Helper()
	if p == nil {
		p = map[string]any{}
	}
	p["board"] = f.board
	r, e := client.BoardCall(context.Background(), client.Options{URL: f.url, Identity: id}, "", method, p)
	require.NoError(t, e)
	return r
}
func (f *snapshotFixture) version(t *testing.T, expiry time.Time) (proto.BoardSnapshotVersion, [][]byte) {
	t.Helper()
	parts := [][]byte{[]byte("recipient-encrypted first part"), []byte("recipient-encrypted tail")}
	payload := bytes.Join(parts, nil)
	sum := sha256.Sum256(payload)
	v := proto.BoardSnapshotVersion{Board: f.board, Item: proto.NewEnvelopeID(), Version: proto.NewEnvelopeID(), Epoch: 1, PayloadSHA256: hex.EncodeToString(sum[:]), PayloadBytes: int64(len(payload)), Recipients: []string{f.owner.ID()}, ExpiresAt: expiry, Metadata: []byte("encrypted manifest")}
	ciphertext := [][]byte{}
	for _, part := range parts {
		blob := proto.NewEnvelopeID()
		raw, e := proto.SealBoardContent(f.key, f.board, 1, blob, part)
		require.NoError(t, e)
		sum := sha256.Sum256(raw)
		v.Chunks = append(v.Chunks, proto.BoardSnapshotChunk{Blob: blob, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(raw))})
		ciphertext = append(ciphertext, raw)
	}
	v.Sign(f.owner)
	require.NoError(t, v.Verify())
	return v, ciphertext
}
func (f *snapshotFixture) upload(t *testing.T, v proto.BoardSnapshotVersion, raw [][]byte) {
	t.Helper()
	for i, data := range raw {
		_, e := client.BoardSnapshotBlob(context.Background(), client.Options{URL: f.url, Identity: f.owner}, v, i, data)
		require.NoError(t, e)
	}
}
func (f *snapshotFixture) publish(t *testing.T, v proto.BoardSnapshotVersion) {
	t.Helper()
	r := f.call(t, f.owner, "snapshots.publish", map[string]any{"snapshot": v})
	require.Equal(t, 200, r.Status, r.Error)
}
func (f *snapshotFixture) member(t *testing.T, role string) *proto.Identity {
	t.Helper()
	id, e := proto.NewIdentity()
	require.NoError(t, e)
	token := proto.NewEnvelopeID()
	r := f.call(t, f.owner, "invites.create", map[string]any{"epoch": 1, "token": token, "ttl_seconds": 120, "role": role, "key_package": "encrypted"})
	require.Equal(t, 200, r.Status, r.Error)
	ws, _ := boardSocket(t, f.url, id, token)
	require.NoError(t, ws.Close())
	return id
}

func TestBoardSnapshotHardExpiryPinsAndLegacyOptIn(t *testing.T) {
	f := newSnapshotFixture(t)
	expiry := time.Unix(0, f.now.Load()).UTC().Add(time.Hour)
	v, raw := f.version(t, expiry)
	f.upload(t, v, raw)
	f.publish(t, v)
	require.Equal(t, 200, f.call(t, f.owner, "boards.capabilities", nil).Status)
	var board struct {
		Capabilities []string `json:"capabilities"`
	}
	require.NoError(t, json.Unmarshal(f.call(t, f.owner, "boards.get", nil).Body, &board))
	require.Contains(t, board.Capabilities, proto.BoardSnapshotsCapability)
	var catalog struct {
		Items     []proto.BoardItemVersion     `json:"items"`
		Snapshots []proto.BoardSnapshotVersion `json:"snapshots"`
	}
	r := f.call(t, f.owner, "items.list", nil)
	require.Equal(t, 200, r.Status)
	require.NoError(t, json.Unmarshal(r.Body, &catalog))
	require.Empty(t, catalog.Items)
	require.Empty(t, catalog.Snapshots)
	r = f.call(t, f.owner, "items.list", map[string]any{"include_snapshots": true})
	require.NoError(t, json.Unmarshal(r.Body, &catalog))
	require.Len(t, catalog.Snapshots, 1)
	f.now.Store(expiry.Add(-time.Nanosecond).UnixNano())
	for i, data := range raw {
		got, e := client.BoardSnapshotBlob(context.Background(), client.Options{URL: f.url, Identity: f.owner}, v, i, nil)
		require.NoError(t, e)
		require.Equal(t, data, got)
	}
	require.Equal(t, 200, f.call(t, f.owner, "pins.set", map[string]any{"item": v.Item, "version_id": v.Version}).Status)
	require.Equal(t, 200, f.call(t, f.owner, "snapshots.get", map[string]any{"item": v.Item, "version_id": v.Version}).Status)
	f.now.Store(expiry.UnixNano())
	require.Equal(t, 404, f.call(t, f.owner, "snapshots.get", map[string]any{"item": v.Item, "version_id": v.Version}).Status)
	require.Equal(t, 404, f.call(t, f.owner, "pins.set", map[string]any{"item": v.Item, "version_id": v.Version}).Status)
	for i := range raw {
		_, e := client.BoardSnapshotBlob(context.Background(), client.Options{URL: f.url, Identity: f.owner}, v, i, nil)
		require.Error(t, e)
	}
	for _, method := range []string{"items.list", "items.versions"} {
		r = f.call(t, f.owner, method, map[string]any{"item": v.Item, "include_snapshots": true})
		catalog.Snapshots = nil
		require.NoError(t, json.Unmarshal(r.Body, &catalog))
		require.Empty(t, catalog.Snapshots)
	}
	r = f.call(t, f.owner, "pins.list", map[string]any{"include_snapshots": true})
	require.JSONEq(t, `{"pins":[],"next_cursor":""}`, string(r.Body))
	require.NoError(t, f.store.SweepBoardBlobs())
	for _, c := range v.Chunks {
		_, e := os.Stat(filepath.Join(f.dir, "board-blobs", f.board, c.Blob))
		require.ErrorIs(t, e, os.ErrNotExist)
	}
}

func TestBoardSnapshotValidationDeleteAndCleanupRetry(t *testing.T) {
	f := newSnapshotFixture(t)
	v, raw := f.version(t, time.Unix(0, f.now.Load()).UTC().Add(time.Hour))
	f.upload(t, v, raw)
	wrong := v
	wrong.ExpiresAt = wrong.ExpiresAt.Add(time.Second)
	wrong.Sign(f.owner)
	require.Equal(t, 409, f.call(t, f.owner, "snapshots.publish", map[string]any{"snapshot": wrong}).Status)
	wrong = v
	wrong.Chunks = append([]proto.BoardSnapshotChunk(nil), v.Chunks...)
	wrong.Chunks[1].SHA256 = strings.Repeat("0", 64)
	wrong.Sign(f.owner)
	require.Equal(t, 409, f.call(t, f.owner, "snapshots.publish", map[string]any{"snapshot": wrong}).Status)
	f.publish(t, v)
	require.Equal(t, 409, f.call(t, f.owner, "snapshots.publish", map[string]any{"snapshot": v}).Status)
	other := f.member(t, "publisher")
	reader := f.member(t, "reader")
	for _, id := range []*proto.Identity{other, reader} {
		require.Equal(t, 403, f.call(t, id, "items.delete", map[string]any{"item": v.Item, "version_id": v.Version}).Status)
	}
	require.Equal(t, 200, f.call(t, f.owner, "pins.set", map[string]any{"item": v.Item, "version_id": v.Version}).Status)
	// A filesystem cleanup failure must still revoke reads and retain retry state.
	path := filepath.Join(f.dir, "board-blobs", f.board, v.Chunks[0].Blob)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(path, "blocked"), []byte("x"), 0600))
	require.Equal(t, 503, f.call(t, f.owner, "items.delete", map[string]any{"item": v.Item, "version_id": v.Version}).Status)
	require.Equal(t, 404, f.call(t, f.owner, "snapshots.get", map[string]any{"item": v.Item, "version_id": v.Version}).Status)
	_, e := client.BoardSnapshotBlob(context.Background(), client.Options{URL: f.url, Identity: f.owner}, v, 1, nil)
	require.Error(t, e)
	require.NoError(t, os.Remove(filepath.Join(path, "blocked")))
	require.NoError(t, os.Remove(path))
	require.Equal(t, 200, f.call(t, f.owner, "items.delete", map[string]any{"item": v.Item, "version_id": v.Version}).Status)
	require.Equal(t, 200, f.call(t, f.owner, "items.delete", map[string]any{"item": v.Item, "version_id": v.Version}).Status)
	for _, c := range v.Chunks {
		_, e = os.Stat(filepath.Join(f.dir, "board-blobs", f.board, c.Blob))
		require.ErrorIs(t, e, os.ErrNotExist)
	}
	require.Equal(t, 409, f.call(t, f.owner, "snapshots.publish", map[string]any{"snapshot": v}).Status)
}

func TestBoardSnapshotLiveSweeperUsesInjectedTime(t *testing.T) {
	f := newSnapshotFixtureWithConfig(t, hub.Config{FramesPerMinute: 10000, BoardSweepInterval: 10 * time.Millisecond})
	expiry := time.Unix(0, f.now.Load()).UTC().Add(time.Hour)
	v, raw := f.version(t, expiry)
	f.upload(t, v, raw)
	f.publish(t, v)
	require.Equal(t, 200, f.call(t, f.owner, "pins.set", map[string]any{"item": v.Item, "version_id": v.Version}).Status)
	f.now.Store(expiry.UnixNano())
	require.Eventually(t, func() bool {
		for _, c := range v.Chunks {
			if _, e := os.Stat(filepath.Join(f.dir, "board-blobs", f.board, c.Blob)); !os.IsNotExist(e) {
				return false
			}
		}
		return true
	}, time.Second, 5*time.Millisecond)
}

func TestBoardSnapshotMixedCatalogPaginationKeepsLegacyReadersWorking(t *testing.T) {
	f := newSnapshotFixture(t)
	for range 9 {
		v, raw := f.version(t, time.Unix(0, f.now.Load()).UTC().Add(time.Hour))
		f.upload(t, v, raw)
		f.publish(t, v)
	}
	for range 8 {
		blob := proto.NewEnvelopeID()
		data, e := proto.SealBoardContent(f.key, f.board, 1, blob, []byte("legacy config"))
		require.NoError(t, e)
		sum := sha256.Sum256(data)
		v := proto.BoardItemVersion{Board: f.board, Item: proto.NewEnvelopeID(), Version: proto.NewEnvelopeID(), Epoch: 1, Blob: blob, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data)), Metadata: []byte("legacy encrypted metadata")}
		v.Sign(f.owner)
		_, e = client.BoardBlob(context.Background(), client.Options{URL: f.url, Identity: f.owner}, v, data)
		require.NoError(t, e)
		require.Equal(t, 200, f.call(t, f.owner, "items.publish", map[string]any{"version": v}).Status)
	}
	var page struct {
		Items     []proto.BoardItemVersion     `json:"items"`
		Snapshots []proto.BoardSnapshotVersion `json:"snapshots"`
		Cursor    string                       `json:"next_cursor"`
	}
	r := f.call(t, f.owner, "items.list", nil)
	require.Equal(t, 200, r.Status)
	require.NoError(t, json.Unmarshal(r.Body, &page))
	require.Len(t, page.Items, 8)
	require.Empty(t, page.Snapshots)
	for _, v := range page.Items {
		require.NoError(t, v.Verify())
	}
	cursor := ""
	seen := map[string]bool{}
	snapshots := 0
	for {
		r = f.call(t, f.owner, "items.list", map[string]any{"include_snapshots": true, "cursor": cursor})
		require.Equal(t, 200, r.Status)
		page.Items = nil
		page.Snapshots = nil
		require.NoError(t, json.Unmarshal(r.Body, &page))
		require.LessOrEqual(t, len(page.Items)+len(page.Snapshots), 8)
		for _, v := range page.Items {
			require.False(t, seen[v.Item])
			seen[v.Item] = true
			require.NoError(t, v.Verify())
		}
		for _, v := range page.Snapshots {
			require.False(t, seen[v.Item])
			seen[v.Item] = true
			require.NoError(t, v.Verify())
			snapshots++
		}
		if page.Cursor == "" {
			break
		}
		require.Greater(t, page.Cursor, cursor)
		cursor = page.Cursor
	}
	require.Len(t, seen, 17)
	require.Equal(t, 9, snapshots)
}
