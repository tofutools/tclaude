package agentd_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
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

// conv is the principal's conv, for grants the operator would make.
func (s *externalStewardFlow) conv() string {
	s.t.Helper()
	stewards, err := db.ListExternalStewards()
	require.NoError(s.t, err)
	require.Len(s.t, stewards, 1)
	return stewards[0].ConvID
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
		stewardtools.ReadBoard:   {"id": "plan"},
		stewardtools.CreateBoard: {"title": "plan"},
		stewardtools.UpdateBoard: {"id": "plan", "elements": []map[string]any{}},
		stewardtools.SetTaskLink: {"agent": "worker"},
		stewardtools.SpawnAgent:  {"group": "crew"},
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
// one, can actually be held, and comes with a level unless it is opt-in. A tool on
// the never-list could only fail.
func TestExternalStewardMCP_ListedPermissionsCanBeHeld(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Steward.Writes = config.StewardWritesWorkers
	require.NoError(t, config.Save(cfg))
	issued := s.create("assistant")
	for _, tool := range stewardtools.List() {
		for _, slug := range tool.Permissions {
			assert.Truef(t, agentd.IsKnownPermSlug(slug), "%s lists unknown %s", tool.Name, slug)
			assert.Falsef(t, agentd.ExternalStewardRefusesForTest(slug), "%s lists never-listed %s", tool.Name, slug)
			if slug != agentd.PermDashboardCapture && slug != agentd.PermAgentSpawn {
				assert.Containsf(t, issued.Grants, slug, "%s needs %s, which a new steward lacks", tool.Name, slug)
			}
			assert.Containsf(t, tool.Access, slug, "%s does not tell the operator about %s", tool.Name, slug)
		}
	}
}

func TestExternalStewardMCP_Boards(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	session := s.mcpSession(s.create("assistant").Credential)

	var board struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpOK(t, session, stewardtools.CreateBoard, map[string]any{"title": "Plan"})), &board))
	require.NotEmpty(t, board.ID)
	assert.Contains(t, mcpOK(t, session, stewardtools.UpdateBoard, map[string]any{"id": board.ID, "elements": []map[string]any{
		{"id": "note", "type": "text", "x": 1, "y": 2, "width": 50, "height": 20, "text": "ship it", "version": 1},
	}}), `"accepted":1`)
	assert.Contains(t, mcpOK(t, session, stewardtools.ReadBoard, map[string]any{"id": board.ID}), "ship it")
	assert.Contains(t, mcpOK(t, session, stewardtools.ListBoards, nil), board.ID)
}

// With no page open each dashboard tool says so, rather than pretend.
func TestExternalStewardMCP_NoDashboardIsPlain(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	session := s.mcpSession(issued.Credential)
	agentd.ResetDashboardGuideForTest()
	t.Cleanup(agentd.ResetDashboardGuideForTest)

	assert.Contains(t, mcpOK(t, session, stewardtools.DashboardView, nil), `"known":false`)
	assert.Contains(t, mcpOK(t, session, stewardtools.DashboardCue, map[string]any{"tab": "groups"}), `"dashboard_on_screen":false`)

	refused, isError := mcpCall(t, session, stewardtools.DashboardCapture, nil)
	require.True(t, isError)
	assert.Contains(t, refused, agentd.PermDashboardCapture, "off until the operator grants it")

	require.NoError(t, db.GrantAgentPermissionWithScope(s.conv(), agentd.PermDashboardCapture, "", "test"))
	closed, isError := mcpCall(t, session, stewardtools.DashboardCapture, nil)
	require.True(t, isError)
	assert.Contains(t, closed, "no dashboard")
}

// The operator is told who is looking and pointing, by the name they gave it.
func TestExternalStewardMCP_DashboardNamesTheAssistant(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	require.NoError(t, db.GrantAgentPermissionWithScope(s.conv(), agentd.PermDashboardCapture, "", "test"))
	session := s.mcpSession(issued.Credential)
	agentd.ResetDashboardGuideForTest()
	t.Cleanup(agentd.ResetDashboardGuideForTest)
	dash := agentd.BuildDashboardHandlerForTest()
	type capture struct {
		ID        string `json:"id"`
		AgentName string `json:"agent_name"`
		Form      bool   `json:"form"`
	}
	ping := func() capture {
		rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost, "/api/companion/presence",
			map[string]any{"client": "tab", "focused": true, "visible": true}))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp struct {
			Capture capture `json:"capture"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp.Capture
	}
	present := func(visible bool) {
		rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost, "/api/companion/presence",
			map[string]any{"client": "tab", "focused": visible, "visible": visible}))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	present(false)
	assert.Contains(t, mcpOK(t, session, stewardtools.DashboardCue, map[string]any{"tab": "groups"}), `"dashboard_on_screen":false`,
		"a tab in the background is open but nobody sees it")
	agentd.ResetDashboardGuideForTest()
	present(true)
	assert.Contains(t, mcpOK(t, session, stewardtools.DashboardCue, map[string]any{"tab": "groups"}), `"dashboard_on_screen":true`)
	snapReq := testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil)
	snapReq.Header.Set("Origin", "http://127.0.0.1:0")
	snapshot := testharness.Serve(dash, snapReq)
	require.Equal(t, http.StatusOK, snapshot.Code, snapshot.Body.String())
	var snap struct {
		Cues []struct {
			AgentID   string `json:"agent_id"`
			AgentName string `json:"agent_name"`
		} `json:"companion_cues"`
	}
	require.NoError(t, json.Unmarshal(snapshot.Body.Bytes(), &snap))
	require.Len(t, snap.Cues, 1)
	assert.Equal(t, "assistant", snap.Cues[0].AgentName)
	assert.Equal(t, issued.AgentID, snap.Cues[0].AgentID)

	result := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: stewardtools.DashboardCapture})
		assert.NoError(t, err)
		result <- res
	}()
	var asked capture
	require.Eventually(t, func() bool {
		asked = ping()
		return asked.ID != ""
	}, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, "assistant", asked.AgentName)
	assert.False(t, asked.Form, "the tool never asks for form text")
	answer := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost, "/api/companion/capture/"+asked.ID, map[string]any{
		"png": capturePNG, "context": map[string]any{"tab": "groups"},
	}))
	require.Equal(t, http.StatusNoContent, answer.Code, answer.Body.String())
	got := <-result
	require.False(t, got.IsError)
	require.Len(t, got.Content, 2)
	image, ok := got.Content[0].(*mcp.ImageContent)
	require.True(t, ok, "%T", got.Content[0])
	want, err := base64.StdEncoding.DecodeString(capturePNG)
	require.NoError(t, err)
	assert.Equal(t, want, image.Data)
	assert.Equal(t, "image/png", image.MIMEType)
	assert.Contains(t, got.Content[1].(*mcp.TextContent).Text, "groups")
}

func TestExternalStewardMCP_TaskLink(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	session := s.mcpSession(issued.Credential)
	s.worker("worker-conv-1")
	args := map[string]any{"agent": "worker-conv-1", "url": "https://example.com/issue/1"}

	refused, isError := mcpCall(t, session, stewardtools.SetTaskLink, args)
	require.True(t, isError)
	assert.Contains(t, refused, agentd.PermAgentTask)

	require.NoError(t, db.GrantAgentPermissionWithScope(s.conv(), agentd.PermAgentTask, "", "test"))
	mcpOK(t, session, stewardtools.SetTaskLink, args)
	read := func() string {
		code, body := s.call(issued.Credential, http.MethodGet, "/v1/agent/worker-conv-1/task", nil)
		require.Equal(t, http.StatusOK, code, body)
		return body
	}
	assert.Contains(t, read(), "example.com/issue/1")
	mcpOK(t, session, stewardtools.SetTaskLink, map[string]any{"agent": "worker-conv-1"})
	assert.NotContains(t, read(), "example.com/issue/1", "leaving the link out clears it")
}
