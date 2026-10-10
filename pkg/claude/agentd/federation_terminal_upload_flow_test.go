package agentd_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func terminalImageMultipart(t *testing.T, mimeType string) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="paste.png"`)
	header.Set("Content-Type", mimeType)
	part, err := writer.CreatePart(header)
	require.NoError(t, err)
	_, err = part.Write([]byte("\x89PNG\r\n\x1a\nimage-data"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return body.Bytes(), writer.FormDataContentType()
}

func TestDashboardFederationTerminalImageUploadProxy(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	row := dashboardRemoteTerminalCatalog(t, fh, proto.CapSessionsWatch, proto.CapSessionsAttach)
	raw, contentType := terminalImageMultipart(t, "image/png")
	path := "/api/federation/terminal?peer=" + fh.peer.id.ID() + "&agent=" + row.Agent + "&mode=interactive"
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest("POST", "/api/federation/terminal-attachments?terminal="+url.QueryEscape(path), bytes.NewReader(raw))
		req.Header.Set("Content-Type", contentType)
		done <- testharness.Serve(agentd.BuildDashboardHandlerForTest(), req)
	}()
	var env *proto.Envelope
	fedEventually(t, "image upload open", func() bool {
		es := fh.peer.envelopes(proto.KindTerminalUpload)
		if len(es) > 0 {
			env = es[len(es)-1]
			return true
		}
		return false
	})
	var request struct {
		bundletransfer.Request
		Descriptor  bundletransfer.Descriptor
		Target      proto.SessionOpenPayload
		ContentType string `json:"content_type"`
	}
	require.NoError(t, env.DecodePayload(&request))
	require.Empty(t, request.Descriptor.Inline)
	require.False(t, request.Target.ReadOnly)
	require.Equal(t, row.Incarnation, request.Target.Incarnation)
	require.Equal(t, "builders", request.Target.Group)
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	answer := bundletransfer.Answer{Request: request.Request, OK: true}
	answer.Key = kp.Pub
	reply := fh.peer.envelope(proto.KindBundleAnswer, proto.Endpoint{}, answer)
	reply.InReplyTo = env.ID
	reply.From.Agent = ""
	fh.peer.send(reply)
	conn := fedPeerStream(t, fh.peer, request.Stream, kp, request.Key, false)
	defer conn.Close()
	received, err := io.ReadAll(conn)
	require.NoError(t, err)
	require.Equal(t, raw, received)
	require.NoError(t, request.Descriptor.Verify(received))
	response := `{"status":200,"body":{"token":"remote-token","dir":"/remote/images","files":[{"path":"/remote/images/paste.png","name":"paste.png","size":18}]}}`
	_, err = conn.Write([]byte(response))
	require.NoError(t, err)
	require.NoError(t, conn.CloseWrite())
	rec := <-done
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "/remote/images/paste.png")
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	for _, tail := range []string{"/api/federation/terminal-attachments?terminal=" + url.QueryEscape(strings.Replace(path, "interactive", "watch", 1)), "/api/terminal-file?terminal=" + url.QueryEscape(path) + "&path=/etc/passwd"} {
		req := httptest.NewRequest("POST", tail, bytes.NewReader(raw))
		if strings.Contains(tail, "terminal-file") {
			req.Method = "GET"
		}
		req.Header.Set("Content-Type", contentType)
		rec = testharness.Serve(agentd.BuildDashboardHandlerForTest(), req)
		require.Equal(t, 403, rec.Code, rec.Body.String())
	}
}

func TestFederationTerminalImageUploadTargetAuthorityAndMime(t *testing.T) {
	for _, test := range []struct {
		name, slug, mimeType string
		status               int
	}{{"watch", agentd.PermSessionsWatch, "image/png", 403}, {"attach", agentd.PermSessionsAttach, "image/png", 200}, {"mime", agentd.PermSessionsAttach, "text/html", 415}} {
		t.Run(test.name, func(t *testing.T) {
			fh := newFedHarness(t)
			f, p := fh.f, fh.peer
			root := testutil.CanonicalTempDir(t)
			t.Cleanup(agentd.SetOperatorMessageAttachmentBasesForTest(root, testutil.CanonicalTempDir(t)))
			const conv = "remote-image-target"
			f.HaveGroup("team")
			f.HaveConvWithTitle(conv, "image-target")
			f.HaveMember("team", conv)
			f.HaveAliveSession(conv, "image-runtime", "tclaude-image-runtime", f.TestCwd("work"))
			aid, err := db.AgentIDForConv(conv)
			require.NoError(t, err)
			original := clcommon.Default
			mock := &terminalTmux{Tmux: original, options: map[string]string{}, pane: "%1", windows: "1", version: "tmux 3.4"}
			clcommon.Default = mock
			t.Cleanup(func() { agentd.ResetFederationForTest(); clcommon.Default = original })
			incarnation := terminalCatalogIncarnation(t, fh, aid)
			rec := fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": test.slug, "scope": "group=team"})
			require.Equal(t, 200, rec.Code)
			raw, contentType := terminalImageMultipart(t, test.mimeType)
			d := bundletransfer.New(bundletransfer.Type{Name: "terminal-image"}, raw, "", time.Now().Add(time.Minute))
			d.Inline = nil
			fedGrantConfig(t, fh)
			prior := fedSendOffer(t, p, fedOfferedConfig(t, "Existing offer must survive image ID collision"))
			require.Equal(t, proto.AckAccepted, fedAckFor(t, p, prior.ID).Status)
			d.ID = prior.ID
			t.Cleanup(func() {
				rec := fedHuman(t, f, "POST", "/v1/federation/bundle-offers/"+prior.ID+"/import", nil)
				require.Equal(t, 200, rec.Code, "ready configuration payload survives image-ID reuse: "+rec.Body.String())
			})
			kp, err := stream.NewKeyPair()
			require.NoError(t, err)
			sid := proto.NewEnvelopeID()
			request := bundletransfer.Request{Offer: d.ID, Stream: sid, SHA256: d.SHA256, Key: kp.Pub}
			target := proto.SessionOpenPayload{Agent: aid, Session: "image-runtime", Group: "team", Incarnation: incarnation}
			requestEnvelope := p.envelope(proto.KindTerminalUpload, proto.Endpoint{}, struct {
				bundletransfer.Request
				Descriptor  bundletransfer.Descriptor `json:"descriptor"`
				Target      proto.SessionOpenPayload  `json:"target"`
				ContentType string                    `json:"content_type"`
			}{request, d, target, contentType})
			requestEnvelope.From.Agent = ""
			p.send(requestEnvelope)
			var answer bundletransfer.Answer
			fedEventually(t, "upload admission", func() bool {
				for _, env := range p.envelopes(proto.KindBundleAnswer) {
					var a bundletransfer.Answer
					if env.DecodePayload(&a) == nil && a.Stream == sid {
						answer = a
						return true
					}
				}
				return false
			})
			if test.status == 403 {
				require.False(t, answer.OK)
				require.Contains(t, answer.Reason, "not granted")
				entries, err := os.ReadDir(root)
				require.NoError(t, err)
				require.Empty(t, entries)
				return
			}
			require.True(t, answer.OK, answer.Reason)
			conn := fedPeerStream(t, p, sid, kp, answer.Key, true)
			defer conn.Close()
			_, err = conn.Write(raw)
			require.NoError(t, err)
			require.NoError(t, conn.CloseWrite())
			response, err := io.ReadAll(conn)
			require.NoError(t, err)
			var result struct {
				Status int
				Body   struct {
					Dir   string
					Files []struct{ Path string }
				}
			}
			require.NoError(t, json.Unmarshal(response, &result))
			require.Equal(t, test.status, result.Status, string(response))
			if test.status == 200 {
				require.Len(t, result.Body.Files, 1)
				stored, err := os.ReadFile(result.Body.Files[0].Path)
				require.NoError(t, err)
				require.Equal(t, []byte("\x89PNG\r\n\x1a\nimage-data"), stored)
				require.True(t, strings.HasPrefix(result.Body.Dir, root+string(os.PathSeparator)))
			} else {
				entries, err := os.ReadDir(root)
				require.NoError(t, err)
				require.Empty(t, entries)
			}
			entries, err := db.ListAuditLog(db.AuditLogFilter{Verb: "sessions.attach.upload"})
			require.NoError(t, err)
			require.NotEmpty(t, entries)
		})
	}
}
