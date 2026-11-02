package mcpserve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/stewardtools"
)

// fakeDaemon answers over a real local listener, for what the flow tests in
// agentd can't reach: which headers leave this process, and timing.
func fakeDaemon(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = nil
	target := strings.TrimPrefix(server.URL, "http://")
	return NewClientWithHTTP(&http.Client{Transport: roundTripTo{target, transport}}, testCredential)
}

type roundTripTo struct {
	host string
	next http.RoundTripper
}

func (r roundTripTo) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Host = r.host
	return r.next.RoundTrip(clone)
}

func connect(t *testing.T, client *Client) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := NewServer(client, "test").Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.NotEmpty(t, result.Content)
	return result.Content[0].(*mcp.TextContent).Text, result.IsError
}

func TestServer_OffersExactlyTheListedTools(t *testing.T) {
	session := connect(t, fakeDaemon(t, func(http.ResponseWriter, *http.Request) {}))
	listed, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	var offered, want []string
	for _, tool := range listed.Tools {
		offered = append(offered, tool.Name)
		assert.NotEmpty(t, tool.Description, tool.Name)
	}
	for _, tool := range stewardtools.List() {
		want = append(want, tool.Name)
	}
	assert.ElementsMatch(t, want, offered)
}

// The assistant's environment may well hold the operator token or a session
// claim. Neither may reach the daemon: the credential is the only identity sent.
func TestClient_SendsTheCredentialAndNoOtherIdentity(t *testing.T) {
	t.Setenv("TCLAUDE_HUMAN_TOKEN", "tclo_operatortoken")
	t.Setenv("TCLAUDE_SESSION_ID", "claimed-session")
	var seen http.Header
	session := connect(t, fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	}))
	_, isError := callTool(t, session, stewardtools.Whoami, nil)
	require.False(t, isError)
	assert.Equal(t, testCredential, seen.Get(agentipc.ExternalStewardCredentialHeader))
	for name := range seen {
		if strings.HasPrefix(strings.ToLower(name), "x-tclaude") {
			assert.Equal(t, http.CanonicalHeaderKey(agentipc.ExternalStewardCredentialHeader), name)
		}
	}
	assert.NotContains(t, strings.ToLower(headerText(seen)), "operatortoken")
}

func headerText(h http.Header) string {
	raw, _ := json.Marshal(h)
	return string(raw)
}

func TestTool_DaemonRefusalIsAToolError(t *testing.T) {
	session := connect(t, fakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"permission_denied","error":"needs fleet.read"}`))
	}))
	text, isError := callTool(t, session, stewardtools.FleetBrief, nil)
	assert.True(t, isError)
	assert.Contains(t, text, "needs fleet.read")
	assert.NotContains(t, text, testCredential)
}

func TestTool_ArgumentsReachTheRoute(t *testing.T) {
	var paths []string
	var bodies []map[string]any
	session := connect(t, fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.String())
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) == nil {
			bodies = append(bodies, body)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	callTool(t, session, stewardtools.ListAgents, map[string]any{"group": "a b&c"})
	callTool(t, session, stewardtools.Inbox, map[string]any{"unread_only": true, "limit": 5000})
	callTool(t, session, stewardtools.ReadMessage, map[string]any{"id": 7})
	callTool(t, session, stewardtools.SendMessage, map[string]any{"to": "worker", "body": "go"})
	callTool(t, session, stewardtools.Reply, map[string]any{"id": 7, "body": "ok"})
	callTool(t, session, stewardtools.NotifyHuman, map[string]any{"subject": "s", "body": "b"})
	assert.Equal(t, []string{
		"GET /v1/peers?group=a+b%26c",
		"GET /v1/inbox?limit=100&unread=1",
		"GET /v1/messages/7",
		"POST /v1/messages",
		"POST /v1/messages/7/reply",
		"POST /v1/notify-human",
	}, paths)
	require.Len(t, bodies, 3)
	assert.Equal(t, "worker", bodies[0]["to"])
	assert.Equal(t, "ok", bodies[1]["body"])
	assert.Equal(t, "b", bodies[2]["body"])
}

type activityDaemon struct {
	inbox, attention atomic.Pointer[string]
	fleetStatus      atomic.Int32
}

func (d *activityDaemon) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/inbox" {
		_, _ = w.Write([]byte(*d.inbox.Load()))
		return
	}
	if status := d.fleetStatus.Load(); status != 0 {
		w.WriteHeader(int(status))
		_, _ = w.Write([]byte(`{"code":"auth","error":"needs fleet.read"}`))
		return
	}
	_, _ = w.Write([]byte(`{"attention":` + *d.attention.Load() + `}`))
}

func newActivityDaemon(t *testing.T) (*activityDaemon, *mcp.ClientSession) {
	t.Helper()
	prev := activityPollInterval
	activityPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { activityPollInterval = prev })
	d := &activityDaemon{}
	none := "[]"
	d.inbox.Store(&none)
	d.attention.Store(&none)
	return d, connect(t, fakeDaemon(t, d.handler))
}

func wait(t *testing.T, session *mcp.ClientSession, args map[string]any) waitResult {
	t.Helper()
	text, isError := callTool(t, session, stewardtools.WaitForActivity, args)
	require.False(t, isError, text)
	var result waitResult
	require.NoError(t, json.Unmarshal([]byte(text), &result))
	return result
}

func TestWaitForActivity_NoCursorAnswersAtOnce(t *testing.T) {
	d, session := newActivityDaemon(t)
	inbox := `[{"id":4,"subject":"old"}]`
	d.inbox.Store(&inbox)
	first := wait(t, session, nil)
	assert.True(t, first.Changed)
	require.Len(t, first.Messages, 1)
	assert.NotEmpty(t, first.Cursor)
}

func TestWaitForActivity_ReturnsOnlyWhatIsNewAndWakesOnMail(t *testing.T) {
	d, session := newActivityDaemon(t)
	inbox := `[{"id":4}]`
	d.inbox.Store(&inbox)
	cursor := wait(t, session, nil).Cursor

	idle := wait(t, session, map[string]any{"cursor": cursor, "timeout_seconds": 1})
	assert.False(t, idle.Changed)
	assert.Empty(t, idle.Messages, "mail already reported is not repeated while unread")
	assert.Equal(t, cursor, idle.Cursor)

	go func() {
		time.Sleep(50 * time.Millisecond)
		next := `[{"id":5,"subject":"new"},{"id":4}]`
		d.inbox.Store(&next)
	}()
	woken := wait(t, session, map[string]any{"cursor": cursor, "timeout_seconds": 10})
	assert.True(t, woken.Changed)
	require.Len(t, woken.Messages, 1)
	assert.EqualValues(t, 5, woken.Messages[0].ID)
	assert.NotEqual(t, cursor, woken.Cursor)
}

func TestWaitForActivity_WakesOnAttentionButNotOnAnIdleClock(t *testing.T) {
	d, session := newActivityDaemon(t)
	attention := func(kind, detail string) *string {
		s := `[{"kind":"` + kind + `","agent_id":"agt_1","detail":"` + detail + `"}]`
		return &s
	}
	d.attention.Store(attention(IdleAttentionKind, "has been idle for 1m"))
	cursor := wait(t, session, nil).Cursor

	d.attention.Store(attention(IdleAttentionKind, "has been idle for 2m"))
	assert.False(t, wait(t, session, map[string]any{"cursor": cursor, "timeout_seconds": 1}).Changed)

	d.attention.Store(attention("messages", "1 unread message in Messages"))
	cursor = wait(t, session, nil).Cursor
	d.attention.Store(attention("messages", "2 unread messages in Messages"))
	assert.True(t, wait(t, session, map[string]any{"cursor": cursor, "timeout_seconds": 1}).Changed, "a rising count")

	none := "[]"
	d.attention.Store(&none)
	assert.True(t, wait(t, session, map[string]any{"cursor": cursor, "timeout_seconds": 1}).Changed)
}

// fleet.read can be taken away; mail still has to work.
func TestWaitForActivity_WithoutFleetReadWatchesMail(t *testing.T) {
	d, session := newActivityDaemon(t)
	d.fleetStatus.Store(http.StatusForbidden)
	first := wait(t, session, nil)
	assert.Contains(t, first.AttentionUnavailable, "fleet.read")
	assert.False(t, wait(t, session, map[string]any{"cursor": first.Cursor, "timeout_seconds": 1}).Changed)
	inbox := `[{"id":9}]`
	d.inbox.Store(&inbox)
	assert.True(t, wait(t, session, map[string]any{"cursor": first.Cursor, "timeout_seconds": 1}).Changed)
}

// A revoked credential must end the wait, not look like quiet.
func TestWaitForActivity_RefusalEndsTheWait(t *testing.T) {
	session := connect(t, fakeDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"external_steward_auth","error":"revoked"}`))
	}))
	text, isError := callTool(t, session, stewardtools.WaitForActivity, map[string]any{"cursor": "0.x", "timeout_seconds": 10})
	assert.True(t, isError)
	assert.Contains(t, text, "external_steward_auth")
}

func TestWaitForActivity_RefusesAForeignCursor(t *testing.T) {
	_, session := newActivityDaemon(t)
	_, isError := callTool(t, session, stewardtools.WaitForActivity, map[string]any{"cursor": "nonsense"})
	assert.True(t, isError)
}

func TestToolList_NamesAreUnique(t *testing.T) {
	names := []string{}
	for _, tool := range stewardtools.List() {
		assert.False(t, slices.Contains(names, tool.Name), tool.Name)
		names = append(names, tool.Name)
	}
}
