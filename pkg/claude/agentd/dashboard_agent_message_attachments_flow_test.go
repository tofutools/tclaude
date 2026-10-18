package agentd_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/startupctx"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0x0d, 'I', 'H', 'D', 'R'}

type mailboxAttachmentMsg struct {
	ID          int64  `json:"id"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
	Attachments []struct {
		ID          int64  `json:"id"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		SizeBytes   int64  `json:"size_bytes"`
		URL         string `json:"url"`
		Previewable bool   `json:"previewable"`
	} `json:"attachments"`
}

func mailboxWithAttachments(t *testing.T, dash http.Handler, id string) map[string]mailboxAttachmentMsg {
	t.Helper()
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, "/api/mailbox?id="+id, nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var payload struct {
		Messages []mailboxAttachmentMsg `json:"messages"`
	}
	testharness.DecodeJSON(t, rec, &payload)
	out := map[string]mailboxAttachmentMsg{}
	for _, m := range payload.Messages {
		out[m.Subject] = m
	}
	return out
}

// Scenario: a startup briefing lists its spawn uploads under "Attached
// files". The Messages reader gets the one inside the daemon's private
// attachment root as a previewable, downloadable file, with the body text
// untouched; a path outside the roots and a symlink leading out of one stay
// plain text and are refused by the download route.
func TestDashboardMailbox_StartupBriefingFilesAreServed(t *testing.T) {
	f := newFlow(t)
	f.HaveConvWithTitle(mbAlice, "alice")

	batch := filepath.Join(tclcommon.SpawnAttachmentsPrivateDir("sess-alice"), "batch-1")
	require.NoError(t, os.MkdirAll(batch, 0o700))
	image := filepath.Join(batch, "image.png")
	require.NoError(t, os.WriteFile(image, pngBytes, 0o600))
	outside := filepath.Join(testutil.CanonicalTempDir(t), "secret.txt")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))
	escape := filepath.Join(batch, "escape.txt")
	require.NoError(t, os.Symlink(outside, escape))

	body := "Your task brief:\n\nlook at this\n\n---\n\n" + startupctx.AttachmentsSection([]string{outside, image, escape})
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		ToConv: mbAlice, Subject: db.StartupContextSubject, Body: body, ToRecipients: []string{mbAlice},
	})
	require.NoError(t, err)

	dash := dashHandlerForTest(t)
	msg := mailboxWithAttachments(t, dash, mbAlice)[db.StartupContextSubject]
	assert.Equal(t, body, msg.Body, "the body keeps its paths")
	require.Len(t, msg.Attachments, 1, "only the file inside the private root is offered")
	got := msg.Attachments[0]
	assert.Equal(t, "image.png", got.Filename)
	assert.Equal(t, "image/png", got.ContentType)
	assert.Equal(t, int64(len(pngBytes)), got.SizeBytes)
	assert.True(t, got.Previewable)
	assert.Equal(t, int64(-2), got.ID, "a briefing file's id is the negative of its 1-based position in the body")
	assert.Equal(t, fmt.Sprintf("/api/agent-messages/%d/attachments/-2", id), got.URL)

	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, got.URL, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, pngBytes, rec.Body.Bytes())
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))

	for _, attachment := range []int{-1, -3} {
		rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet,
			fmt.Sprintf("/api/agent-messages/%d/attachments/%d", id, attachment), nil))
		assert.Equal(t, http.StatusGone, rec.Code, "file %d is outside the attachment roots", attachment)
		assert.NotContains(t, rec.Body.String(), "secret")
	}
	// A sender writing the same section itself names no files: operator mail
	// under the briefing's own subject, and ordinary mail under any other.
	spoofed, err := db.InsertAgentMessage(&db.AgentMessage{
		ToConv: mbAlice, Subject: db.StartupContextSubject, Body: body, ToRecipients: []string{mbAlice}, OperatorAuthored: true,
	})
	require.NoError(t, err)
	_, err = db.InsertAgentMessage(&db.AgentMessage{
		ToConv: mbAlice, Subject: "look", Body: body, ToRecipients: []string{mbAlice},
	})
	require.NoError(t, err)
	for _, m := range mailboxWithAttachments(t, dash, mbAlice) {
		if m.ID != id {
			assert.Empty(t, m.Attachments, "message %d (%s) is not a briefing tclaude wrote", m.ID, m.Subject)
		}
	}
	rec = testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet,
		fmt.Sprintf("/api/agent-messages/%d/attachments/-2", spoofed), nil))
	assert.Equal(t, http.StatusNotFound, rec.Code, "the route honours only tclaude's own briefings")

	for _, attachment := range []int{-4, 0} {
		rec = testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet,
			fmt.Sprintf("/api/agent-messages/%d/attachments/%d", id, attachment), nil))
		assert.Equal(t, http.StatusNotFound, rec.Code, "attachment %d", attachment)
	}
}

// Scenario: mail stored with attachment rows (operator mail, federated mail)
// carries them in the mailbox API, and each downloads only under its own
// message id.
func TestDashboardMailbox_StoredAgentMessageAttachmentsAreServed(t *testing.T) {
	f := newFlow(t)
	f.HaveConvWithTitle(mbAlice, "alice")
	path := filepath.Join(testutil.CanonicalTempDir(t), "notes.md")
	require.NoError(t, os.WriteFile(path, []byte("# notes\n"), 0o600))
	id, err := db.InsertAgentMessageWithAttachments(&db.AgentMessage{
		ToConv: mbAlice, Subject: "with file", Body: "see attached", ToRecipients: []string{mbAlice}, OperatorAuthored: true,
	}, []db.AgentMessageAttachment{{Filename: "notes.md", ContentType: "text/markdown", SizeBytes: 8, StoragePath: path}})
	require.NoError(t, err)
	other, err := db.InsertAgentMessage(&db.AgentMessage{ToConv: mbAlice, Subject: "plain", Body: "no file", ToRecipients: []string{mbAlice}})
	require.NoError(t, err)

	dash := dashHandlerForTest(t)
	msgs := mailboxWithAttachments(t, dash, mbAlice)
	assert.Empty(t, msgs["plain"].Attachments)
	require.Len(t, msgs["with file"].Attachments, 1)
	got := msgs["with file"].Attachments[0]
	assert.Equal(t, "notes.md", got.Filename)
	assert.Equal(t, fmt.Sprintf("/api/agent-messages/%d/attachments/%d", id, got.ID), got.URL)

	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, got.URL, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "# notes\n", rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "notes.md")

	rec = testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet,
		fmt.Sprintf("/api/agent-messages/%d/attachments/%d", other, got.ID), nil))
	assert.Equal(t, http.StatusNotFound, rec.Code, "an attachment id does not reach across messages")
}
