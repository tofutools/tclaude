package agentd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func TestBoardSnapshotRecipientsAreMembersOnly(t *testing.T) {
	self, err := proto.NewIdentity()
	require.NoError(t, err)
	peer, err := proto.NewIdentity()
	require.NoError(t, err)
	forged, err := proto.NewIdentity()
	require.NoError(t, err)
	outsider, err := proto.NewIdentity()
	require.NoError(t, err)
	members := []boardMember{{Instance: self.ID(), Pub: self.Pub}, {Instance: peer.ID(), Pub: peer.Pub}, {Instance: outsider.ID(), Pub: forged.Pub}}
	peers := []db.FederationPeer{{InstanceID: peer.ID(), Label: "lab"}}

	instances, recipients, err := boardSnapshotRecipients(self, members, []string{"lab"}, peers)
	require.NoError(t, err)
	assert.Len(t, recipients, 2, "the publisher is always a recipient")
	assert.ElementsMatch(t, []string{self.ID(), peer.ID()}, instances)
	assert.IsIncreasing(t, instances)

	_, _, err = boardSnapshotRecipients(self, members, []string{outsider.ID()}, peers)
	assert.Error(t, err, "a member whose key does not match its id is refused")
	_, _, err = boardSnapshotRecipients(self, members, []string{"nobody"}, peers)
	assert.Error(t, err)
	_, _, err = boardSnapshotRecipients(self, members, nil, peers)
	assert.Error(t, err)
}

func TestBoardSnapshotChunks(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	board := proto.NewEnvelopeID()
	payload := bytes.Repeat([]byte("x"), proto.MaxBoardItemBytes+10)
	chunks, err := splitBoardSnapshot(payload)
	require.NoError(t, err)
	require.Len(t, chunks, 2)
	blobs := []string{proto.NewEnvelopeID(), proto.NewEnvelopeID()}
	ciphers := make([][]byte, len(chunks))
	want := make([]boardSnapshotChunk, len(chunks))
	for i, c := range chunks {
		ciphers[i], err = proto.SealBoardContent(key, board, 1, blobs[i], c)
		require.NoError(t, err)
		sum := sha256.Sum256(ciphers[i])
		want[i] = boardSnapshotChunk{SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(ciphers[i]))}
	}
	whole := sha256.Sum256(payload)
	digest := hex.EncodeToString(whole[:])
	read := func(order ...int) func(int) ([]byte, error) {
		return func(i int) ([]byte, error) { return ciphers[order[i]], nil }
	}
	open := func(i int, c []byte) ([]byte, error) { return proto.OpenBoardContent(key, board, 1, blobs[i], c) }

	var out bytes.Buffer
	require.NoError(t, assembleBoardSnapshot(&out, want, digest, int64(len(payload)), read(0, 1), open))
	assert.Equal(t, payload, out.Bytes())

	assert.Error(t, assembleBoardSnapshot(&bytes.Buffer{}, want, digest, int64(len(payload)), read(1, 0), open), "reordered chunks are refused")
	assert.Error(t, assembleBoardSnapshot(&bytes.Buffer{}, []boardSnapshotChunk{want[0], want[0]}, digest, 2*(want[0].Bytes-boardSealOverhead), read(0, 0), open), "a duplicated chunk fails the payload digest")
	assert.Error(t, assembleBoardSnapshot(&bytes.Buffer{}, want[:1], digest, int64(len(payload)), read(0), open), "missing chunks are refused")
	assert.Error(t, assembleBoardSnapshot(&bytes.Buffer{}, want, digest, int64(len(payload))+1, read(0, 1), open))

	_, err = splitBoardSnapshot(make([]byte, boardSnapshotMaxBytes+1))
	assert.Error(t, err)
}
