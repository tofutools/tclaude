package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const (
	attachSender   = "attach-send-aaaa-bbbb-1111"
	attachReceiver = "attach-recv-aaaa-bbbb-2222"
	attachCopied   = "attach-copy-aaaa-bbbb-3333"
)

func newAttachmentTeam(t *testing.T) {
	t.Helper()
	f := newFlow(t)
	f.HaveGroup("team")
	for conv, title := range map[string]string{attachSender: "sender", attachReceiver: "receiver", attachCopied: "copied"} {
		f.HaveMember("team", conv)
		f.HaveConvWithTitle(conv, title)
	}
	t.Cleanup(agentd.SetOperatorMessageAttachmentsBaseForTest(t.TempDir()))
}

func sendAsAgent(t *testing.T, from, path string, body map[string]any) (int, []byte) {
	t.Helper()
	rec := testharness.Serve(agentd.BuildHandlerForTest(), agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, path, body), from))
	agentd.WaitForBackgroundForTest()
	return rec.Code, rec.Body.Bytes()
}

func readAttachments(t *testing.T, conv string, id int64) []db.AgentMessageAttachment {
	t.Helper()
	rec := testharness.Serve(agentd.BuildHandlerForTest(), agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/messages/"+strconv.FormatInt(id, 10), nil), conv))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var detail struct {
		Attachments []db.AgentMessageAttachment `json:"attachments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
	return detail.Attachments
}

func TestLocalAgentMessageCarriesAttachmentToItsReader(t *testing.T) {
	newAttachmentTeam(t)
	code, raw := sendAsAgent(t, attachSender, "/v1/messages", map[string]any{
		"to": "receiver", "body": "see the mock",
		"attachments": []proto.AttachmentPayload{{Name: "mock.png", Data: []byte("png bytes")}},
	})
	require.Equal(t, http.StatusOK, code, "body=%s", raw)
	var sent struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(raw, &sent))

	got := readAttachments(t, attachReceiver, sent.ID)
	require.Len(t, got, 1)
	assert.Equal(t, "mock.png", got[0].Filename)
	assert.Equal(t, "image/png", got[0].ContentType)
	data, err := os.ReadFile(got[0].StoragePath)
	require.NoError(t, err)
	assert.Equal(t, "png bytes", string(data))

	// A reply carries a file back the same way.
	code, raw = sendAsAgent(t, attachReceiver, "/v1/messages/"+strconv.FormatInt(sent.ID, 10)+"/reply", map[string]any{
		"body": "marked up", "attachments": []proto.AttachmentPayload{{Name: "notes.md", Data: []byte("# notes")}},
	})
	require.Equal(t, http.StatusOK, code, "body=%s", raw)
	var reply struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(raw, &reply))
	back := readAttachments(t, attachSender, reply.ID)
	require.Len(t, back, 1)
	assert.Equal(t, "notes.md", back[0].Filename)
}

func TestCcRecipientsEachGetTheirOwnCopyOfAnAttachment(t *testing.T) {
	newAttachmentTeam(t)
	code, raw := sendAsAgent(t, attachSender, "/v1/messages", map[string]any{
		"to": "receiver", "cc": []string{"copied"}, "body": "both of you",
		"attachments": []proto.AttachmentPayload{{Name: "mock.png", Data: []byte("png bytes")}},
	})
	require.Equal(t, http.StatusOK, code, "body=%s", raw)
	var sent struct {
		Recipients []struct {
			ConvID    string `json:"conv_id"`
			MessageID int64  `json:"message_id"`
		} `json:"recipients"`
	}
	require.NoError(t, json.Unmarshal(raw, &sent))
	require.Len(t, sent.Recipients, 2)
	first := readAttachments(t, sent.Recipients[0].ConvID, sent.Recipients[0].MessageID)
	second := readAttachments(t, sent.Recipients[1].ConvID, sent.Recipients[1].MessageID)
	require.Len(t, first, 1)
	require.Len(t, second, 1)
	assert.NotEqual(t, first[0].StoragePath, second[0].StoragePath)
}

func TestLocalAgentAttachmentsKeepTheMessageLimits(t *testing.T) {
	newAttachmentTeam(t)
	cases := map[string]struct {
		files []proto.AttachmentPayload
		to    string
		want  string
	}{
		"too many files": {
			to:    "receiver",
			files: []proto.AttachmentPayload{{Name: "a.txt", Data: []byte("a")}, {Name: "b.txt", Data: []byte("b")}, {Name: "c.txt", Data: []byte("c")}, {Name: "d.txt", Data: []byte("d")}, {Name: "e.txt", Data: []byte("e")}},
			want:  "at most",
		},
		"too large": {
			to:    "receiver",
			files: []proto.AttachmentPayload{{Name: "big.png", Data: []byte(strings.Repeat("x", proto.MaxAttachmentBytes+1))}},
			want:  "limit",
		},
		"type not allowed": {
			to:    "receiver",
			files: []proto.AttachmentPayload{{Name: "run.sh", Data: []byte("#!/bin/sh")}},
			want:  "type not allowed",
		},
		"a name cannot leave the message's directory": {
			to:    "receiver",
			files: []proto.AttachmentPayload{{Name: "../../data/db.sqlite", Data: []byte("x")}},
			want:  "type not allowed",
		},
		"group mail stays text": {
			to:    "group:team",
			files: []proto.AttachmentPayload{{Name: "a.txt", Data: []byte("a")}},
			want:  "group messages",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, raw := sendAsAgent(t, attachSender, "/v1/messages", map[string]any{"to": tc.to, "body": "x", "attachments": tc.files})
			assert.Equal(t, http.StatusBadRequest, code, "body=%s", raw)
			assert.Contains(t, string(raw), tc.want)
		})
	}
}
