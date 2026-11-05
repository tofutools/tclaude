package agentipc

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortSocketDir is a temp dir short enough for a unix socket path. (The
// agentipctest helper imports this package, so it cannot be used here.)
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tclbp")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func serveBroker(t *testing.T, sock string, status int, reply string) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
}

// A dial candidate nothing listens on is passed over for the next one, which
// is how a current client still reaches an older daemon on a legacy path.
func TestPostToBroker_DialsTheFirstSocketThatAnswers(t *testing.T) {
	dir := shortSocketDir(t)
	live := filepath.Join(dir, "live.sock")
	serveBroker(t, live, http.StatusOK, `{"ok":true}`)

	var out struct{ OK bool }
	err := postToBrokerAt([]string{filepath.Join(dir, "absent.sock"), live}, "/v1/whoami/hook", []byte(`{}`), time.Second, &out)

	require.NoError(t, err)
	assert.True(t, out.OK)
}

func TestPostToBroker_AnotherStatusIsABrokerStatusError(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "a.sock")
	serveBroker(t, sock, http.StatusTooManyRequests, "  slow down\n")

	err := postToBrokerAt([]string{sock}, "/v1/whoami/statusline", []byte(`{}`), time.Second, &struct{}{})

	statusErr, ok := errors.AsType[*BrokerStatusError](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, http.StatusTooManyRequests, statusErr.Status)
	assert.Equal(t, "agentd returned 429: slow down", err.Error())
}

func TestPostToBroker_NoSocketPathIsAnError(t *testing.T) {
	err := postToBrokerAt(nil, "/v1/whoami/hook", []byte(`{}`), time.Second, &struct{}{})

	assert.EqualError(t, err, "no agentd socket path resolved")
}
