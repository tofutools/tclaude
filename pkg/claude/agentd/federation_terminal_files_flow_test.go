package agentd_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestFederationTerminalFilePinnedProjectRead(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	root := testutil.CanonicalTempDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "report.txt"), []byte("verified report"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "empty"), nil, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".env"), []byte("secret"), 0600))
	require.NoError(t, os.Mkdir(filepath.Join(root, ".ssh"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".ssh", "id_rsa"), []byte("private"), 0600))
	require.NoError(t, os.Symlink("report.txt", filepath.Join(root, "alias")))
	const conv = "file-target"
	f.HaveGroup("team")
	f.HaveConvWithTitle(conv, "files")
	f.HaveMember("team", conv)
	f.HaveAliveSession(conv, "file-runtime", "tclaude-file-runtime", root)
	aid, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	original := clcommon.Default
	clcommon.Default = &terminalTmux{Tmux: original, options: map[string]string{}, pane: "%1", windows: "1", version: "tmux 3.4"}
	t.Cleanup(func() { agentd.ResetFederationForTest(); clcommon.Default = original })
	incarnation := terminalCatalogIncarnation(t, fh, aid)
	grant := func(slug string) {
		rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": slug, "scope": "group=team"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	grant(agentd.PermSessionsWatch)
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	open := proto.SessionOpenPayload{Agent: aid, Session: "file-runtime", Incarnation: incarnation, Group: "team", Stream: proto.NewEnvelopeID(), Key: kp.Pub, ReadOnly: true, Cols: 80, Rows: 24}
	p.send(p.envelope(proto.KindSessionOpen, proto.Endpoint{}, open))
	a := terminalAnswer(t, p, open.Stream)
	require.True(t, a.OK, a.Reason)
	require.False(t, a.Files)
	terminalConn := fedPeerStream(t, p, open.Stream, kp, a.Key, true)
	defer terminalConn.Close()
	// A file read addresses the already-live viewer, never a freshly discovered agent.
	read := func(path string, head bool) (int, string, []byte) {
		key, err := stream.NewKeyPair()
		require.NoError(t, err)
		sid := proto.NewEnvelopeID()
		req := struct {
			bundletransfer.Request
			Viewer string `json:"viewer"`
			Path   string `json:"path"`
			Head   bool   `json:"head"`
		}{bundletransfer.Request{Offer: open.Stream, Stream: sid, Key: key.Pub}, open.Stream, path, head}
		env := p.envelope(proto.KindTerminalFile, proto.Endpoint{}, req)
		env.From.Agent = ""
		p.send(env)
		var answer bundletransfer.Answer
		fedEventually(t, "file answer", func() bool {
			for _, e := range p.envelopes(proto.KindBundleAnswer) {
				var a bundletransfer.Answer
				if e.DecodePayload(&a) == nil && a.Stream == sid {
					answer = a
					return true
				}
			}
			return false
		})
		if !answer.OK {
			return 403, answer.Reason, nil
		}
		conn := fedPeerStream(t, p, sid, key, answer.Key, true)
		defer conn.Close()
		require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
		var prefix [4]byte
		_, err = io.ReadFull(conn, prefix[:])
		require.NoError(t, err)
		n := binary.BigEndian.Uint32(prefix[:])
		require.Less(t, n, uint32(4097))
		raw := make([]byte, n)
		_, err = io.ReadFull(conn, raw)
		require.NoError(t, err)
		var h struct {
			Status int
			Code   string
			Bytes  int64
			SHA256 string
		}
		require.NoError(t, json.Unmarshal(raw, &h))
		body, err := io.ReadAll(conn)
		require.NoError(t, err)
		if h.Status == 200 && !head {
			require.Equal(t, h.Bytes, int64(len(body)))
			hash := sha256.Sum256(body)
			require.Equal(t, h.SHA256, hex.EncodeToString(hash[:]))
		}
		if head {
			require.Empty(t, body)
		}
		return h.Status, h.Code, body
	}
	status, code, _ := read("report.txt", false)
	require.Equal(t, 403, status)
	require.Equal(t, "not_shared", code)
	grant(agentd.PermSessionsFilesRead)
	status, _, body := read("report.txt", false)
	require.Equal(t, 200, status)
	require.Equal(t, "verified report", string(body))
	status, _, _ = read("report.txt", true)
	require.Equal(t, 200, status)
	status, _, body = read("empty", false)
	require.Equal(t, 200, status)
	require.Empty(t, body)
	for _, path := range []string{"../report.txt", ".env", ".ssh/id_rsa", "alias", "/etc/passwd"} {
		for _, head := range []bool{false, true} {
			status, code, _ = read(path, head)
			require.Equal(t, 403, status, path)
			require.Equal(t, "unsafe_path", code, path)
		}
	}
	rec := fedHuman(t, f, "DELETE", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermSessionsFilesRead, "scope": "group=team"})
	require.Equal(t, 200, rec.Code)
	status, code, _ = read("report.txt", false)
	require.Equal(t, 403, status)
	require.Equal(t, "not_shared", code)
}
