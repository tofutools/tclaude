package hub

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testutil"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBoardDeletionRemovesCiphertextImmediately(t *testing.T) {
	for _, mode := range []string{"owner", "moderator"} {
		t.Run(mode, func(t *testing.T) {
			dir := testutil.CanonicalTempDir(t)
			s, err := OpenStore(filepath.Join(dir, "hub.sqlite"))
			require.NoError(t, err)
			defer s.Close()
			owner, err := proto.NewIdentity()
			require.NoError(t, err)
			board := proto.NewEnvelopeID()
			call := func(method string, p any) (any, error) {
				raw, err := json.Marshal(p)
				require.NoError(t, err)
				return s.boardCall(owner.ID(), owner.Pub, &proto.BoardRequest{ID: proto.NewEnvelopeID(), Method: method, Payload: raw, ExpiresAt: time.Now().Add(time.Minute)}, true)
			}
			_, err = call("boards.create", map[string]any{"board": board, "name": "temporary", "key_proofs": map[string][]byte{owner.ID(): make([]byte, 32)}, "envelopes": map[string]any{owner.ID(): map[string]any{"ct": "opaque"}}})
			require.NoError(t, err)
			data := []byte("encrypted fixture")
			sum := sha256.Sum256(data)
			require.NoError(t, s.StoreBoardBlob(owner.ID(), board, proto.NewEnvelopeID(), hex.EncodeToString(sum[:]), int64(len(data)), bytes.NewReader(data)))
			storage := filepath.Join(dir, "board-blobs", board)
			require.DirExists(t, storage)
			outside := filepath.Join(dir, "keep")
			require.NoError(t, os.WriteFile(outside, []byte("keep"), 0600))
			require.NoError(t, os.Symlink(outside, filepath.Join(storage, "alias")))
			if mode == "owner" {
				_, err = call("boards.delete", map[string]any{"board": board})
			} else {
				raw, _ := json.Marshal(map[string]string{"board": board})
				_, err = s.moderateBoard("boards.delete", raw)
			}
			require.NoError(t, err)
			require.NoDirExists(t, storage)
			require.FileExists(t, outside)
			var n int
			require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM board_blobs WHERE board=?`, board).Scan(&n))
			require.Zero(t, n)
		})
	}
}
