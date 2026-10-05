package stream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// pair returns the two ends of one websocket, keyed as initiator and
// responder of stream sid.
func pair(t *testing.T) (ini, res *Conn) {
	t.Helper()
	const sid = "env_0123456789abcdef"
	ka, err := NewKeyPair()
	require.NoError(t, err)
	kb, err := NewKeyPair()
	require.NoError(t, err)
	server := make(chan *websocket.Conn, 1)
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err == nil {
			server <- ws
		}
	}))
	t.Cleanup(srv.Close)
	cws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	sws := <-server

	k1, err := DeriveKeys(ka, kb.Pub, sid, true)
	require.NoError(t, err)
	k2, err := DeriveKeys(kb, ka.Pub, sid, false)
	require.NoError(t, err)
	ini, err = New(cws, k1)
	require.NoError(t, err)
	res, err = New(sws, k2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ini.Close(); _ = res.Close() })
	return ini, res
}

func TestStream_RoundTripAndOrderlyClose(t *testing.T) {
	a, b := pair(t)
	big := strings.Repeat("x", 3*maxChunk+17)
	go func() {
		_, _ = a.Write([]byte(big))
		_ = a.CloseWrite()
	}()
	got, err := io.ReadAll(b)
	require.NoError(t, err)
	require.Equal(t, big, string(got))

	_, err = b.Write([]byte("reply"))
	require.NoError(t, err)
	require.NoError(t, b.CloseWrite())
	got, err = io.ReadAll(a)
	require.NoError(t, err)
	require.Equal(t, "reply", string(got))

	// Both finished: closing one end ends the other's Drain without a reset.
	go func() { _ = a.Close() }()
	require.ErrorIs(t, b.Drain(), io.ErrUnexpectedEOF)
}

func TestStream_CloseWithoutFinIsAReset(t *testing.T) {
	a, b := pair(t)
	_, err := a.Write([]byte("partial upl"))
	require.NoError(t, err)
	require.NoError(t, a.Close())
	buf := make([]byte, 64)
	n, err := b.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "partial upl", string(buf[:n]))
	_, err = b.Read(buf)
	require.ErrorIs(t, err, ErrReset)
}

func TestStream_ResetAfterFinReachesDrain(t *testing.T) {
	a, b := pair(t)
	// b finishes its direction; a reads EOF and drains.
	require.NoError(t, b.CloseWrite())
	_, err := io.ReadAll(a)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- a.Drain() }()
	// a's direction is still open, so the end of the websocket is
	// reported, never swallowed.
	require.NoError(t, b.Close())
	require.ErrorIs(t, <-done, io.ErrUnexpectedEOF)
}

func TestStream_TruncationIsNotEOF(t *testing.T) {
	a, b := pair(t)
	_, err := a.Write([]byte("abc"))
	require.NoError(t, err)
	require.NoError(t, a.ws.Close()) // relay drops without FIN or reset
	got, err := io.ReadAll(b)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Equal(t, "abc", string(got))
}

func TestStream_WrongKeysFailAuthentication(t *testing.T) {
	a, b := pair(t)
	b.recv, b.send = b.send, b.recv // as if keys were swapped or reflected
	_, err := a.Write([]byte("hello"))
	require.NoError(t, err)
	_, err = b.Read(make([]byte, 8))
	require.ErrorIs(t, err, ErrTampered)
}
