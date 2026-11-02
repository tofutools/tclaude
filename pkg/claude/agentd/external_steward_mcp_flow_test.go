package agentd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/stewardtools"
	"github.com/tofutools/tclaude/pkg/claude/mcpserve"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// mcpSession runs the real MCP server against the real daemon routes, over a Unix
// socket through the production identity layer. Only the stdio pipe is in memory.
func (s *externalStewardFlow) mcpSession(credential string) *mcp.ClientSession {
	s.t.Helper()
	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	server := mcpserve.NewServer(mcpserve.NewClientWithHTTP(s.client, credential), "test")
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(s.t, err)
	s.t.Cleanup(func() { _ = serverSession.Close() })
	session, err := mcp.NewClient(&mcp.Implementation{Name: "flow", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	require.NoError(s.t, err)
	s.t.Cleanup(func() { _ = session.Close() })
	return session
}

func mcpCall(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	require.NoError(t, err)
	require.NotEmpty(t, result.Content)
	return result.Content[0].(*mcp.TextContent).Text, result.IsError
}

func mcpOK(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	text, isError := mcpCall(t, session, tool, args)
	require.Falsef(t, isError, "%s: %s", tool, text)
	return text
}

func (s *externalStewardFlow) worker(conv string) func(body string) *httptest.ResponseRecorder {
	s.t.Helper()
	s.f.HaveConvWithTitle(conv, "worker")
	s.f.HaveAliveSession(conv, "workers", "tclaude-workers", s.f.TestCwd("w"))
	require.NoError(s.t, db.GrantAgentPermissionWithScope(conv, agentd.PermMessageDirect, "", "test"))
	return func(body string) *httptest.ResponseRecorder {
		return testharness.Serve(s.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(s.t, http.MethodPost, "/v1/messages",
			map[string]any{"to": "assistant", "body": body}), conv))
	}
}

func TestExternalStewardMCP_EveryToolWorksAgainstTheDaemon(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	send := s.worker("worker-conv-1")
	session := s.mcpSession(issued.Credential)

	assert.Contains(t, mcpOK(t, session, stewardtools.Whoami, nil), "assistant")
	assert.Contains(t, mcpOK(t, session, stewardtools.FleetBrief, nil), "worker")
	mcpOK(t, session, stewardtools.ListAgents, nil)

	require.Equal(t, http.StatusOK, send("first report").Code)
	var inbox []struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpOK(t, session, stewardtools.Inbox, map[string]any{"unread_only": true})), &inbox))
	require.Len(t, inbox, 1)
	var waited struct {
		Changed  bool   `json:"changed"`
		Cursor   string `json:"cursor"`
		Messages []struct {
			ID int64 `json:"id"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpOK(t, session, stewardtools.WaitForActivity, nil)), &waited))
	require.Len(t, waited.Messages, 1)

	assert.Contains(t, mcpOK(t, session, stewardtools.ReadMessage, map[string]any{"id": inbox[0].ID}), "first report")
	mcpOK(t, session, stewardtools.Reply, map[string]any{"id": inbox[0].ID, "body": "received"})
	mcpOK(t, session, stewardtools.SendMessage, map[string]any{"to": "worker-conv-1", "body": "next task"})
	mcpOK(t, session, stewardtools.NotifyHuman, map[string]any{"body": "worth a look"})

	require.Equal(t, http.StatusOK, send("second report").Code)
	require.NoError(t, json.Unmarshal([]byte(mcpOK(t, session, stewardtools.WaitForActivity,
		map[string]any{"cursor": waited.Cursor, "timeout_seconds": 5})), &waited))
	assert.True(t, waited.Changed)
	require.Len(t, waited.Messages, 1)
	assert.Greater(t, waited.Messages[0].ID, inbox[0].ID)
}

// Without a pane nothing delivers mail, so reading has to count or the queue fills.
func TestExternalStewardMCP_ReadingMakesRoomInTheQueue(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	send := s.worker("worker-conv-1")
	session := s.mcpSession(issued.Credential)
	for i := range agentd.RegularAgentMessageQueueLimitForTest {
		require.Equal(t, http.StatusOK, send(fmt.Sprintf("report %d", i)).Code)
	}
	require.Equal(t, http.StatusTooManyRequests, send("one too many").Code)
	var inbox []struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpOK(t, session, stewardtools.Inbox, map[string]any{"unread_only": true})), &inbox))
	require.NotEmpty(t, inbox)
	mcpOK(t, session, stewardtools.ReadMessage, map[string]any{"id": inbox[0].ID})
	assert.Equal(t, http.StatusOK, send("room again").Code)
}

// The cap counts unread mail, so the unread view has to reach all of it, however
// much newer mail has been read since.
func TestExternalStewardMCP_OldUnreadMailStaysInView(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	send := s.worker("worker-conv-1")
	session := s.mcpSession(issued.Credential)
	require.Equal(t, http.StatusOK, send("skipped").Code)
	var inbox []struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpOK(t, session, stewardtools.Inbox, map[string]any{"unread_only": true})), &inbox))
	require.Len(t, inbox, 1)
	skipped := inbox[0].ID
	for i := range 3 {
		require.Equal(t, http.StatusOK, send(fmt.Sprintf("later %d", i)).Code)
		require.NoError(t, json.Unmarshal([]byte(mcpOK(t, session, stewardtools.Inbox, map[string]any{"limit": 1})), &inbox))
		mcpOK(t, session, stewardtools.ReadMessage, map[string]any{"id": inbox[0].ID})
	}
	require.NoError(t, json.Unmarshal([]byte(mcpOK(t, session, stewardtools.Inbox,
		map[string]any{"unread_only": true, "limit": 1})), &inbox))
	require.Len(t, inbox, 1)
	assert.Equal(t, skipped, inbox[0].ID)
}

func TestExternalStewardMCP_RevokedCredentialFailsEveryTool(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	session := s.mcpSession(issued.Credential)
	mcpOK(t, session, stewardtools.Whoami, nil)
	code, body := s.humanPost("/v1/external-stewards/assistant/revoke", map[string]any{})
	require.Equal(t, http.StatusOK, code, body)
	args := map[string]map[string]any{
		stewardtools.ReadMessage: {"id": 1},
		stewardtools.SendMessage: {"to": "worker", "body": "y"},
		stewardtools.Reply:       {"id": 1, "body": "y"},
		stewardtools.NotifyHuman: {"body": "y"},
	}
	for _, tool := range stewardtools.List() {
		text, isError := mcpCall(t, session, tool.Name, args[tool.Name])
		assert.Truef(t, isError, "%s: %s", tool.Name, text)
		assert.Containsf(t, text, "external_steward_auth", tool.Name)
		assert.NotContains(t, text, issued.Credential)
	}
}

// mcpserve can't import agentd, so its copy of this kind is held here.
func TestExternalStewardMCP_IdleKindMatchesTheFleet(t *testing.T) {
	assert.Equal(t, agentd.FleetIdleLongerForTest, mcpserve.IdleAttentionKind)
}

// What Config tells the operator has to be true: each listed permission is a real
// one, can actually be held, and comes with a default level. A tool on the
// never-list could only fail.
func TestExternalStewardMCP_ListedPermissionsCanBeHeld(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	for _, tool := range stewardtools.List() {
		for _, slug := range tool.Permissions {
			assert.Truef(t, agentd.IsKnownPermSlug(slug), "%s lists unknown %s", tool.Name, slug)
			assert.Falsef(t, agentd.ExternalStewardRefusesForTest(slug), "%s lists never-listed %s", tool.Name, slug)
			assert.Containsf(t, issued.Grants, slug, "%s needs %s, which a new steward lacks", tool.Name, slug)
			assert.Containsf(t, tool.Access, slug, "%s does not tell the operator about %s", tool.Name, slug)
		}
	}
}
