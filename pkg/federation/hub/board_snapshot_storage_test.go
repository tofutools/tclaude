package hub

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestBoardSnapshotUpgradeAndReservationQuotas(t *testing.T) {
	path := filepath.Join(testutil.CanonicalTempDir(t), "hub.sqlite")
	old, e := sql.Open("sqlite", path)
	require.NoError(t, e)
	_, e = old.Exec(schema + adminSchema + boardSchema + boardBlobSchema + boardItemSchema)
	require.NoError(t, e)
	id, e := proto.NewIdentity()
	require.NoError(t, e)
	board := proto.NewEnvelopeID()
	now := time.Now().UTC().Truncate(time.Second)
	_, e = old.Exec(`INSERT INTO boards(id,name,epoch,quota_bytes,created_at) VALUES(?,'legacy',1,8192,?)`, board, ts(now))
	require.NoError(t, e)
	_, e = old.Exec(`INSERT INTO board_members(board,instance,role,pub,key_proof) VALUES(?,?,'owner',?,?)`, board, id.ID(), id.Pub, make([]byte, 32))
	require.NoError(t, e)
	require.NoError(t, old.Close())
	s, e := OpenStoreWithClock(path, func() time.Time { return now })
	require.NoError(t, e)
	defer func() { require.NoError(t, s.Close()) }()
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM boards WHERE id=? AND name='legacy'`, board).Scan(&count))
	require.Equal(t, 1, count)
	// Reservation pressure is checked before reading ciphertext, including uploads
	// that are still pending while another publisher tries to consume the quota.
	data := bytes.Repeat([]byte("x"), 3000)
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	expiry := now.Add(time.Hour)
	first := proto.NewEnvelopeID()
	require.NoError(t, s.StoreBoardSnapshotBlob(id.ID(), board, first, digest, int64(len(data)), expiry, bytes.NewReader(data)))
	blocked := false
	e = s.StoreBoardSnapshotBlob(id.ID(), board, proto.NewEnvelopeID(), digest, 3000, expiry, readerFunc(func(p []byte) (int, error) { blocked = true; return 0, nil }))
	require.Error(t, e)
	require.False(t, blocked, "sub-quota refusal must happen before reading input")

	pendingData := bytes.Repeat([]byte("p"), 1000)
	pendingSum := sha256.Sum256(pendingData)
	pendingSource := bytes.NewReader(pendingData)
	checked := false
	require.NoError(t, s.StoreBoardSnapshotBlob(id.ID(), board, proto.NewEnvelopeID(), hex.EncodeToString(pendingSum[:]), 1000, expiry, readerFunc(func(p []byte) (int, error) {
		if !checked {
			checked = true
			neverRead := false
			err := s.StoreBoardSnapshotBlob(id.ID(), board, proto.NewEnvelopeID(), digest, 100, expiry, readerFunc(func([]byte) (int, error) { neverRead = true; return 0, nil }))
			require.Error(t, err)
			require.False(t, neverRead, "pending reservations consume the sub-quota")
		}
		return pendingSource.Read(p)
	})))
	// Legacy uploads keep the remaining quota and are not reclassified.
	require.NoError(t, s.StoreBoardBlob(id.ID(), board, proto.NewEnvelopeID(), digest, 3000, bytes.NewReader(data)))
	for _, deadline := range []time.Time{time.Time{}, now, now.Add(proto.MaxBoardSnapshotTTL + time.Second)} {
		require.Error(t, s.StoreBoardSnapshotBlob(id.ID(), board, proto.NewEnvelopeID(), digest, 3000, deadline, bytes.NewReader(data)))
	}
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM board_snapshot_blobs WHERE board=?`, board).Scan(&count))
	require.Equal(t, 2, count)
	// Reopening is idempotent and doesn't need a colliding numbered migration.
	other, e := OpenStoreWithClock(path, func() time.Time { return now })
	require.NoError(t, e)
	require.NoError(t, other.Close())
	var problems int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&problems))
	require.Zero(t, problems)
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestBoardSnapshotPublisherAndPendingReservationAuthority(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s, e := OpenStoreWithClock(filepath.Join(testutil.CanonicalTempDir(t), "hub.sqlite"), func() time.Time { return now })
	require.NoError(t, e)
	defer func() { require.NoError(t, s.Close()) }()
	a, e := proto.NewIdentity()
	require.NoError(t, e)
	b, e := proto.NewIdentity()
	require.NoError(t, e)
	board := proto.NewEnvelopeID()
	_, e = s.db.Exec(`INSERT INTO boards(id,name,epoch,created_at) VALUES(?,'test',1,?)`, board, ts(now))
	require.NoError(t, e)
	for _, id := range []*proto.Identity{a, b} {
		_, e = s.db.Exec(`INSERT INTO board_members(board,instance,role,pub,key_proof) VALUES(?,?,'publisher',?,?)`, board, id.ID(), id.Pub, make([]byte, 32))
		require.NoError(t, e)
	}
	data := bytes.Repeat([]byte("x"), 100)
	sum := sha256.Sum256(data)
	blob := proto.NewEnvelopeID()
	expiry := now.Add(time.Hour)
	require.NoError(t, s.StoreBoardSnapshotBlob(a.ID(), board, blob, hex.EncodeToString(sum[:]), 100, expiry, bytes.NewReader(data)))
	v := proto.BoardSnapshotVersion{Board: board, Item: proto.NewEnvelopeID(), Version: proto.NewEnvelopeID(), Epoch: 1, PayloadSHA256: strings.Repeat("0", 64), PayloadBytes: 60, Recipients: []string{b.ID()}, ExpiresAt: expiry, Metadata: []byte("encrypted"), Chunks: []proto.BoardSnapshotChunk{{Blob: blob, SHA256: hex.EncodeToString(sum[:]), Bytes: 100}}}
	v.Sign(b)
	payload, e := json.Marshal(map[string]any{"board": board, "snapshot": v})
	require.NoError(t, e)
	_, e = s.boardCall(b.ID(), b.Pub, &proto.BoardRequest{ID: proto.NewEnvelopeID(), Payload: payload, ExpiresAt: time.Now().Add(time.Minute), Method: "snapshots.publish"}, false)
	require.Error(t, e, "a publisher cannot adopt another publisher's reserved chunks")

	v.Sign(a)
	payload, e = json.Marshal(map[string]any{"board": board, "snapshot": v})
	require.NoError(t, e)
	call := func(id *proto.Identity, method string, payload []byte) (any, error) {
		return s.boardCall(id.ID(), id.Pub, &proto.BoardRequest{ID: proto.NewEnvelopeID(), Payload: payload, ExpiresAt: time.Now().Add(time.Minute), Method: method}, false)
	}
	_, e = call(a, "snapshots.publish", payload)
	require.NoError(t, e)
	del, e := json.Marshal(map[string]any{"board": board, "item": v.Item, "version_id": v.Version})
	require.NoError(t, e)
	_, e = call(b, "items.delete", del)
	require.Error(t, e)
	_, e = call(a, "items.delete", del)
	require.NoError(t, e, "a publisher can delete its own version without owner authority")
	// Expiry during a blocked upload must release its reservation, not resurrect it.
	source := bytes.NewReader(data)
	e = s.StoreBoardSnapshotBlob(a.ID(), board, proto.NewEnvelopeID(), hex.EncodeToString(sum[:]), 100, expiry, readerFunc(func(p []byte) (int, error) { now = expiry; return source.Read(p) }))
	require.Error(t, e)
	var pending int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM board_blobs WHERE state='pending'`).Scan(&pending))
	require.Zero(t, pending)
}

func TestBoardSnapshotAbandonedReservationReleasesQuota(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s, e := OpenStoreWithClock(filepath.Join(testutil.CanonicalTempDir(t), "hub.sqlite"), func() time.Time { return now })
	require.NoError(t, e)
	defer func() { require.NoError(t, s.Close()) }()
	board, blob := proto.NewEnvelopeID(), proto.NewEnvelopeID()
	id, e := proto.NewIdentity()
	require.NoError(t, e)
	_, e = s.db.Exec(`INSERT INTO boards(id,name,epoch,created_at) VALUES(?,'test',1,?)`, board, ts(now))
	require.NoError(t, e)
	_, e = s.db.Exec(`INSERT INTO board_blobs VALUES(?,?,100,?,'pending',?,0)`, board, blob, strings.Repeat("0", 64), ts(now.Add(time.Hour)))
	require.NoError(t, e)
	_, e = s.db.Exec(`INSERT INTO board_snapshot_blobs VALUES(?,?,?,?)`, board, blob, id.ID(), snapshotTS(now.Add(7*24*time.Hour)))
	require.NoError(t, e)
	now = now.Add(2 * time.Hour)
	require.NoError(t, s.SweepBoardBlobs())
	var pending int
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM board_blobs WHERE board=?`, board).Scan(&pending))
	require.Zero(t, pending)
	require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM board_snapshot_blobs WHERE board=?`, board).Scan(&pending))
	require.Zero(t, pending)
}

func TestBoardSnapshotStreamChecksExpiryAfterThrottle(t *testing.T) {
	expiry := time.Now().UTC()
	calls := 0
	output := &bytes.Buffer{}
	bandwidth := &boardBandwidth{rw: output, lim: newByteLimiter(1 << 20), done: make(chan struct{})}
	guard := snapshotTTLWriter{dst: bandwidth, expiry: expiry, now: func() time.Time {
		calls++
		if calls == 1 {
			return expiry.Add(-time.Nanosecond)
		}
		return expiry
	}}
	bandwidth.beforeWrite = guard.check
	n, e := guard.Write([]byte("buffered ciphertext"))
	require.Error(t, e)
	require.Zero(t, n)
	require.Empty(t, output.Bytes(), "expiry crossing the throttle boundary must release no ciphertext")
}
