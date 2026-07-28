package agentd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func TestNormalizeTUIConnectURL(t *testing.T) {
	t.Run("host and port default to HTTP", func(t *testing.T) {
		base, origin, err := normalizeTUIConnectURL("agent-host:8321")
		require.NoError(t, err)
		assert.Equal(t, "http://agent-host:8321", base)
		assert.Equal(t, base, origin)
	})

	t.Run("HTTPS URL keeps a reverse-proxy prefix", func(t *testing.T) {
		base, origin, err := normalizeTUIConnectURL("https://agents.example.test/tclaude/")
		require.NoError(t, err)
		assert.Equal(t, "https://agents.example.test/tclaude", base)
		assert.Equal(t, "https://agents.example.test", origin)
	})

	for name, target := range map[string]string{
		"empty":        " ",
		"wrong scheme": "ssh://agent-host",
		"userinfo":     "https://operator@agent-host",
		"query":        "https://agent-host?token=nope",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := normalizeTUIConnectURL(target)
			require.Error(t, err)
		})
	}
}

func TestRemoteTUIAPIUsesOperatorTokenThenRetainsSessionCookie(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/tui/v1/peers", r.URL.Path)
		assert.Equal(t, srvOrigin(r), r.Header.Get("Origin"))
		assert.Equal(t, "tclo_remote-test", r.Header.Get(agent.HumanTokenHeader))
		switch calls.Add(1) {
		case 1:
			assert.Empty(t, r.Cookies())
			http.SetCookie(w, &http.Cookie{Name: "tclaude_test", Value: "session", Path: "/"})
		case 2:
			cookie, err := r.Cookie("tclaude_test")
			require.NoError(t, err)
			assert.Equal(t, "session", cookie.Value)
		default:
			t.Fatalf("unexpected call")
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	api, err := newRemoteTUIAPI(srv.URL, "tclo_remote-test")
	require.NoError(t, err)
	var rows []tuiAgentRow
	require.NoError(t, api.get("/v1/peers", &rows))
	require.NoError(t, api.get("/v1/peers", &rows))
	assert.Equal(t, int32(2), calls.Load())
}

func srvOrigin(r *http.Request) string {
	return "http://" + r.Host
}

func TestRemoteTUIAPIRemainsReusableAfterAnOutage(t *testing.T) {
	var available atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !available.Load() {
			http.Error(w, "restarting", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`[{"conv_id":"c1","title":"back","online":true}]`))
	}))
	defer srv.Close()

	api, err := newRemoteTUIAPI(srv.URL, "tclo_remote-test")
	require.NoError(t, err)
	var rows []tuiAgentRow
	require.Error(t, api.get("/v1/peers", &rows))

	available.Store(true)
	require.NoError(t, api.get("/v1/peers", &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "back", rows[0].Title)
}

func TestTUIInitialRefreshDoesNotOverlapTheFirstTick(t *testing.T) {
	api, err := newRemoteTUIAPI("agent-host:8321", "tclo_remote-test")
	require.NoError(t, err)
	m := newTUIModel(api)
	assert.True(t, m.refreshing, "Init starts a refresh immediately")

	updated, cmd := m.Update(tuiTickMsg{})
	got := updated.(tuiModel)
	assert.True(t, got.refreshing)
	assert.NotNil(t, cmd, "the tick must reschedule itself without replacing the in-flight refresh")
}

func TestRemoteTUIModelDoesNotClaimHostLocalCapabilities(t *testing.T) {
	api, err := newRemoteTUIAPI("agent-host:8321", "tclo_remote-test")
	require.NoError(t, err)
	m := newTUIModel(api)
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "live", Online: true}}

	assert.True(t, m.operator)
	assert.Empty(t, m.enterHint(), "a remote terminal cannot attach to the daemon host's tmux")
	assert.NotContains(t, m.renderSpawnForm(), "go to its pane")
	assert.NotContains(t, m.renderSpawnForm(), "tab complete dir")

	updated, _ := m.handleKey(tea.KeyPressMsg{Code: 'q', Text: "q"})
	remote := updated.(tuiModel)
	assert.Contains(t, remote.confirmPrompt(), "Quit this remote console?")
	assert.NotContains(t, remote.confirmPrompt(), "shut down agentd")
	assert.Contains(t, remote.renderHelp(), "agentd keeps running")
	assert.Contains(t, remote.renderHelp(), "reconnects when agentd returns")
}

func TestRemoteTUIAPINamesUnsupportedServers(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	api, err := newRemoteTUIAPI(srv.URL, "tclo_remote-test")
	require.NoError(t, err)

	err = api.get("/v1/peers", &[]tuiAgentRow{})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "without remote TUI support"), err)
}

func TestRemoteTUIAPINeverForwardsTheOperatorTokenThroughARedirect(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Store(true)
		assert.Empty(t, r.Header.Get(agent.HumanTokenHeader))
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	api, err := newRemoteTUIAPI(source.URL, "tclo_remote-test")
	require.NoError(t, err)
	err = api.get("/v1/peers", &[]tuiAgentRow{})
	require.Error(t, err)
	assert.False(t, redirected.Load(), "the client must not visit a redirect target at all")
	assert.Contains(t, err.Error(), "Temporary Redirect")
}
