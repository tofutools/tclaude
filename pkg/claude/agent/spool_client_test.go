package agent

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

func newTestSpoolDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(agentipc.SpoolReqDir(dir), 0o700))
	require.NoError(t, os.MkdirAll(agentipc.SpoolRespDir(dir), 0o700))
	return dir
}

// respondOnce is a minimal stand-in for the daemon's spool consumer: it
// polls the req dir for one envelope, hands it to reply, and publishes the
// returned response under the same request id.
func respondOnce(t *testing.T, dir string, reply func(agentipc.SpoolRequest) agentipc.SpoolResponse) {
	t.Helper()
	reqDir := agentipc.SpoolReqDir(dir)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(reqDir)
		require.NoError(t, err)
		for _, e := range entries {
			if !agentipc.SpoolEnvelopeFile(e.Name()) {
				continue
			}
			full := filepath.Join(reqDir, e.Name())
			data, err := os.ReadFile(full)
			require.NoError(t, err)
			require.NoError(t, os.Remove(full))
			env, err := agentipc.DecodeSpoolRequest(data)
			require.NoError(t, err)
			respData, err := agentipc.EncodeSpoolResponse(reply(env))
			require.NoError(t, err)
			id := strings.TrimSuffix(e.Name(), ".json")
			respPath := agentipc.SpoolEnvelopePath(agentipc.SpoolRespDir(dir), id)
			require.NoError(t, agentipc.WriteSpoolFile(respPath, respData))
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Error("respondOnce: no request envelope appeared")
}

func TestSpoolTransportRoundTrip(t *testing.T) {
	dir := newTestSpoolDir(t)
	done := make(chan struct{})
	var seen agentipc.SpoolRequest
	go func() {
		defer close(done)
		respondOnce(t, dir, func(req agentipc.SpoolRequest) agentipc.SpoolResponse {
			seen = req
			return agentipc.SpoolResponse{
				Status: http.StatusCreated,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body:   []byte(`{"ok":true}`),
			}
		})
	}()

	client := NewSpoolHTTPClient(dir, 5*time.Second)
	req, err := http.NewRequest(http.MethodPost, "http://_/v1/messages?x=1",
		strings.NewReader(`{"body":"hello"}`))
	require.NoError(t, err)
	req.Header.Set("Idempotency-Key", "key-1")
	resp, err := client.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	<-done

	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.Equal(t, `{"ok":true}`, string(body))
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	assert.Equal(t, http.MethodPost, seen.Method)
	assert.Equal(t, "/v1/messages?x=1", seen.RequestURI)
	assert.Equal(t, "key-1", seen.Header.Get("Idempotency-Key"))
	assert.Equal(t, `{"body":"hello"}`, string(seen.Body))

	for _, d := range []string{agentipc.SpoolReqDir(dir), agentipc.SpoolRespDir(dir)} {
		entries, err := os.ReadDir(d)
		require.NoError(t, err)
		assert.Empty(t, entries, "a completed exchange must leave no files behind in %s", d)
	}
}

func TestSpoolTransportTimeoutWithdrawsRequest(t *testing.T) {
	dir := newTestSpoolDir(t)
	client := NewSpoolHTTPClient(dir, 200*time.Millisecond)
	_, err := client.Get("http://_/v1/info")
	require.Error(t, err, "no consumer is running, the request must time out")

	entries, err := os.ReadDir(agentipc.SpoolReqDir(dir))
	require.NoError(t, err)
	assert.Empty(t, entries, "a timed-out request must be withdrawn so it cannot execute later")
}
