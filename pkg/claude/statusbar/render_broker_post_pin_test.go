package statusbar

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
)

// These pin postRenderToDaemon's round trip to agentd against a real HTTP
// server on the socket path the client resolves: what it sends, and what
// each answer turns into — a refusal (403, 401) and a throttle (429) are each
// marked as such, anything else that is not a 200 is a plain error carrying
// a preview.

type renderPost struct{ method, path, contentType, body string }

func serveRenderDaemon(t *testing.T, status int, reply string) *[]renderPost {
	t.Helper()
	sock := filepath.Join(agentipctest.ShortSocketDir(t), "a.sock")
	t.Setenv(agentipc.SocketEnv, sock)
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	var got []renderPost
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, renderPost{r.Method, r.URL.Path, r.Header.Get("Content-Type"), string(b)})
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return &got
}

func TestPostRenderToDaemon_SendsTheRequestAndDecodesTheAnswer(t *testing.T) {
	got := serveRenderDaemon(t, http.StatusOK, `{"owned":true,"applied":true,"pinned_window":450000}`)

	resp, err := postRenderToDaemon(BrokeredRenderRequest{ClaimedSessionID: "spwn-x", RenderConvID: "conv-x"})

	require.NoError(t, err)
	assert.Equal(t, BrokeredRenderResponse{Owned: true, Applied: true, PinnedWindow: 450000}, resp)
	require.Len(t, *got, 1)
	assert.Equal(t, http.MethodPost, (*got)[0].method)
	assert.Equal(t, "/v1/whoami/statusline", (*got)[0].path)
	assert.Equal(t, "application/json", (*got)[0].contentType)
	assert.Contains(t, (*got)[0].body, `"conv-x"`)
}

func TestPostRenderToDaemon_EachAnswerItCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name               string
		status             int
		reply              string
		refused, throttled bool
		message            string
	}{
		{"a refusal", http.StatusForbidden, `{"error":"no"}` + "\n", true, false,
			`agentd refused the brokered statusline render: agentd returned 403: {"error":"no"}`},
		{"an unidentified caller", http.StatusUnauthorized, `{"error":"who"}`, true, false,
			`agentd refused the brokered statusline render: agentd returned 401: {"error":"who"}`},
		{"a throttle", http.StatusTooManyRequests, `{"error":"slow"}`, false, true,
			`agentd throttled the brokered statusline render: agentd returned 429: {"error":"slow"}`},
		{"a server error", http.StatusInternalServerError, `boom`, false, false,
			`agentd returned 500: boom`},
		{"an answer that is not JSON", http.StatusOK, `not json`, false, false,
			`decode broker response: invalid character 'o' in literal null (expecting 'u')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serveRenderDaemon(t, tc.status, tc.reply)

			_, err := postRenderToDaemon(BrokeredRenderRequest{})

			require.Error(t, err)
			assert.Equal(t, tc.message, err.Error())
			assert.Equal(t, tc.refused, errors.Is(err, errRenderBrokerRefused))
			assert.Equal(t, tc.throttled, errors.Is(err, errRenderBrokerThrottled))
		})
	}
}

func TestPostRenderToDaemon_NoDaemonIsAnError(t *testing.T) {
	t.Setenv(agentipc.SocketEnv, filepath.Join(agentipctest.ShortSocketDir(t), "none.sock"))

	_, err := postRenderToDaemon(BrokeredRenderRequest{})

	require.Error(t, err)
	assert.False(t, errors.Is(err, errRenderBrokerRefused))
	assert.False(t, errors.Is(err, errRenderBrokerThrottled))
}
