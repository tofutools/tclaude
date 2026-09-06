package transport

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestPublicCorrespondenceAttachmentAndRetirement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	h := testHandler(t, service)
	create := request(h, "POST", "/v2/agents", `{"id":"worker","name":"Worker","task_reference":"task:123","notifications":{"DirectMessage":"none"},"desired":{"Harness":"claude","Model":"fixture","WorkingDirectory":"/tmp/work","Approval":"supervised","Sandbox":"unconfined"}}`, testCredential)
	require.Equal(t, 201, create.Code, create.Body.String())
	var agent model.Agent
	require.NoError(t, json.Unmarshal(create.Body.Bytes(), &agent))
	require.Equal(t, "task:123", agent.TaskReference)
	// An attachment larger than the ordinary 1 MiB JSON mutation limit uses its own bounded claim route.
	content := bytes.Repeat([]byte("a"), 2<<20)
	payload, err := json.Marshal(map[string]any{"filename": "result.txt", "media_type": "text/plain", "content": content})
	require.NoError(t, err)
	uploaded := request(h, "POST", "/v2/attachment-claims", string(payload), testCredential)
	require.Equal(t, 201, uploaded.Code, uploaded.Body.String())
	require.NotContains(t, uploaded.Body.String(), "Owner")
	var claim struct {
		ID         model.AttachmentClaimID
		Attachment model.Attachment
	}
	require.NoError(t, json.Unmarshal(uploaded.Body.Bytes(), &claim))
	send := map[string]any{"request_id": "send", "subject": "Review result", "to": model.MessageAudience{AgentIDs: []model.AgentID{agent.ID}}, "cc": model.MessageAudience{Operator: true}, "body": "Ready", "attachments": []app.AttachmentInput{{Claim: &app.AttachmentClaimReference{ClaimID: claim.ID, AttachmentID: claim.Attachment.ID, Filename: claim.Attachment.Filename, MediaType: claim.Attachment.MediaType, Size: claim.Attachment.Size, SHA256: claim.Attachment.SHA256}}}}
	payload, err = json.Marshal(send)
	require.NoError(t, err)
	sent := request(h, "POST", "/v2/messages", string(payload), testCredential)
	require.Equal(t, 202, sent.Code, sent.Body.String())
	require.NotContains(t, sent.Body.String(), "Generation")
	require.NotContains(t, sent.Body.String(), "Delegation")
	var message model.Message
	require.NoError(t, json.Unmarshal(sent.Body.Bytes(), &message))
	require.Len(t, message.Recipients, 2)
	download := request(h, "GET", "/v2/attachments/"+string(claim.Attachment.ID), "", testCredential)
	require.Equal(t, 200, download.Code)
	var result app.AttachmentContentResult
	require.NoError(t, json.Unmarshal(download.Body.Bytes(), &result))
	require.Equal(t, content, result.Content)
	require.Equal(t, 401, request(h, "GET", "/v2/attachments/"+string(claim.Attachment.ID), "", "").Code)
	retired := request(h, "POST", "/v2/agents/worker/retire", `{"expected_revision":1,"reason":"finished"}`, testCredential)
	require.Equal(t, 200, retired.Code, retired.Body.String())
	require.NotContains(t, retired.Body.String(), "Generation")
	retry := request(h, "POST", "/v2/messages", string(payload), testCredential)
	require.Equal(t, 202, retry.Code, retry.Body.String())
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	h = testHandler(t, app.New(store, providers.NewRegistry()))
	snapshot := request(h, "GET", "/v2/snapshot", "", testCredential)
	require.Equal(t, 200, snapshot.Code)
	require.Contains(t, snapshot.Body.String(), "Review result")
	require.Contains(t, snapshot.Body.String(), "retired")
	require.NotContains(t, snapshot.Body.String(), strings.Repeat("a", 100))
	active := request(h, "POST", "/v2/agents/worker/reactivate", `{"expected_revision":2}`, testCredential)
	require.Equal(t, 200, active.Code, active.Body.String())
}
