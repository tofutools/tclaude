package session

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

// These pin postHookToDaemon, the hook callback's one round trip to agentd,
// against a real HTTP server on the socket path the client resolves: what it
// sends, and what each answer turns into — a refusal (403, 401) is marked as
// one, anything else that is not a 200 is a plain error carrying a preview.

type hookPost struct{ method, path, contentType, body string }

func serveHookDaemon(t *testing.T, status int, reply string) *[]hookPost {
	t.Helper()
	sock := filepath.Join(agentipctest.ShortSocketDir(t), "a.sock")
	t.Setenv(agentipc.SocketEnv, sock)
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	var got []hookPost
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, hookPost{r.Method, r.URL.Path, r.Header.Get("Content-Type"), string(b)})
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return &got
}

func TestPostHookToDaemon_SendsTheBodyAndDecodesTheAnswer(t *testing.T) {
	got := serveHookDaemon(t, http.StatusOK, `{"stdout":"relayed","ack_token":"ack-1"}`)

	resp, err := postHookToDaemon([]byte(`{"claimed_session_id":"spwn-x"}`))

	require.NoError(t, err)
	assert.Equal(t, &BrokeredHookResponse{Stdout: "relayed", AckToken: "ack-1"}, resp)
	assert.Equal(t, []hookPost{{http.MethodPost, "/v1/whoami/hook", "application/json", `{"claimed_session_id":"spwn-x"}`}}, *got)
}

func TestPostHookToDaemon_EachAnswerItCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		reply   string
		refused bool
		message string
	}{
		{"a refusal", http.StatusForbidden, `{"error":"no"}` + "\n", true,
			`agentd refused the brokered hook event: agentd returned 403: {"error":"no"}`},
		{"an unidentified caller", http.StatusUnauthorized, `{"error":"who"}`, true,
			`agentd refused the brokered hook event: agentd returned 401: {"error":"who"}`},
		{"a throttle", http.StatusTooManyRequests, `{"error":"slow"}`, false,
			`agentd returned 429: {"error":"slow"}`},
		{"a server error", http.StatusInternalServerError, `boom`, false,
			`agentd returned 500: boom`},
		{"an answer that is not JSON", http.StatusOK, `not json`, false,
			`decode broker response: invalid character 'o' in literal null (expecting 'u')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serveHookDaemon(t, tc.status, tc.reply)

			resp, err := postHookToDaemon([]byte(`{}`))

			assert.Nil(t, resp)
			require.Error(t, err)
			assert.Equal(t, tc.message, err.Error())
			assert.Equal(t, tc.refused, errors.Is(err, errHookBrokerRefused))
		})
	}
}

func TestPostHookToDaemon_NoDaemonIsAnError(t *testing.T) {
	t.Setenv(agentipc.SocketEnv, filepath.Join(agentipctest.ShortSocketDir(t), "none.sock"))

	resp, err := postHookToDaemon([]byte(`{}`))

	assert.Nil(t, resp)
	require.Error(t, err)
	assert.False(t, errors.Is(err, errHookBrokerRefused))
}
