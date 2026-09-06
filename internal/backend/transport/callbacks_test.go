package transport

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type rawCallbackFunc func(context.Context, ports.RawNativeCallback, ports.RawNativeCallbackResponder) error

func (f rawCallbackFunc) HandleNativeCallback(ctx context.Context, r ports.RawNativeCallback, w ports.RawNativeCallbackResponder) error {
	return f(ctx, r, w)
}

func TestNativeCallbackUsesSeparateCredentialAndActualWrite(t *testing.T) {
	c, err := NewCallbackRegistry("/tmp/callback-fixture.sock")
	require.NoError(t, err)
	defer c.Close()
	secret := strings.Repeat("s", 64)
	digest := sha256.Sum256([]byte(secret))
	calls := 0
	r := ports.CallbackRegistration{RegistrationID: "native_registration_one", ExecutionID: "execution_one", Attempt: 1, CredentialDigest: ports.CallbackCredentialDigest(digest), MaxRequestBytes: 128, MaxResponseBytes: 128,
		Handler: rawCallbackFunc(func(ctx context.Context, raw ports.RawNativeCallback, sink ports.RawNativeCallbackResponder) error {
			calls++
			require.Equal(t, `{"native":"untouched"}`, string(raw.Body))
			result, err := sink.Respond(ctx, ports.RawNativeCallbackResponse{StatusCode: 200, ContentType: "application/json", Body: []byte(`{"guidance":"accepted"}`)})
			require.NoError(t, err)
			require.Equal(t, ports.EffectAccepted, result)
			result, err = sink.Respond(ctx, ports.RawNativeCallbackResponse{})
			require.Error(t, err)
			require.Equal(t, ports.EffectRefused, result)
			return nil
		})}
	binding, err := c.RegisterCallback(context.Background(), r)
	require.NoError(t, err)
	for _, auth := range []string{"", "Bearer " + secret, "Native " + strings.Repeat("x", 64)} {
		req := httptest.NewRequest("POST", binding.Route, strings.NewReader(`{"native":"untouched"}`))
		req.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		c.ServeHTTP(w, req)
		require.Equal(t, 401, w.Code)
	}
	req := httptest.NewRequest("POST", binding.Route, strings.NewReader(`{"native":"untouched"}`))
	req.Header.Set("Authorization", "Native "+secret)
	w := httptest.NewRecorder()
	c.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	require.Equal(t, `{"guidance":"accepted"}`, w.Body.String())
	require.Equal(t, 1, calls)
	require.NoError(t, binding.Cleanup.Close(context.Background()))
	// A stale cleanup object cannot remove a later registration with the same ID.
	r.Attempt = 2
	successor, err := c.RegisterCallback(context.Background(), r)
	require.NoError(t, err)
	require.NoError(t, binding.Cleanup.Close(context.Background()))
	req = httptest.NewRequest("POST", successor.Route, strings.NewReader(`{"native":"untouched"}`))
	req.Header.Set("Authorization", "Native "+secret)
	w = httptest.NewRecorder()
	c.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
	c.Close()
	req = httptest.NewRequest("POST", successor.Route, nil)
	req.Header.Set("Authorization", "Native "+secret)
	w = httptest.NewRecorder()
	c.ServeHTTP(w, req)
	require.Equal(t, 401, w.Code)
}

type failedCallbackWriter struct{ header http.Header }

func (w failedCallbackWriter) Header() http.Header     { return w.header }
func (failedCallbackWriter) WriteHeader(int)           {}
func (failedCallbackWriter) Write([]byte) (int, error) { return 0, errors.New("disconnected") }
func TestNativeCallbackFailedWriteIsUnknown(t *testing.T) {
	c, err := NewCallbackRegistry("/tmp/callback-fixture.sock")
	require.NoError(t, err)
	defer c.Close()
	digest := sha256.Sum256([]byte(strings.Repeat("s", 64)))
	binding, err := c.RegisterCallback(context.Background(), ports.CallbackRegistration{RegistrationID: "native_registration_two", ExecutionID: "execution_two", Attempt: 1, CredentialDigest: ports.CallbackCredentialDigest(digest), MaxRequestBytes: 10, MaxResponseBytes: 10, Handler: rawCallbackFunc(func(ctx context.Context, _ ports.RawNativeCallback, sink ports.RawNativeCallbackResponder) error {
		result, err := sink.Respond(ctx, ports.RawNativeCallbackResponse{StatusCode: 200, ContentType: "text/plain", Body: []byte("guidance")})
		require.Error(t, err)
		require.Equal(t, ports.EffectUnknown, result)
		return err
	})})
	require.NoError(t, err)
	req := httptest.NewRequest("POST", binding.Route, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Native "+strings.Repeat("s", 64))
	c.ServeHTTP(failedCallbackWriter{http.Header{}}, req)
}

func TestNativeCallbackBoundsAndCleanupFence(t *testing.T) {
	c, err := NewCallbackRegistry("/tmp/callback-fixture.sock")
	require.NoError(t, err)
	defer c.Close()
	secret := strings.Repeat("s", 64)
	digest := sha256.Sum256([]byte(secret))
	var binding ports.CallbackBinding
	calls := 0
	binding, err = c.RegisterCallback(context.Background(), ports.CallbackRegistration{RegistrationID: "native_registration_three", ExecutionID: "execution_three", Attempt: 1, CredentialDigest: ports.CallbackCredentialDigest(digest), MaxRequestBytes: 2, MaxResponseBytes: 2, Handler: rawCallbackFunc(func(ctx context.Context, _ ports.RawNativeCallback, sink ports.RawNativeCallbackResponder) error {
		calls++
		require.NoError(t, binding.Cleanup.Close(context.Background()))
		result, err := sink.Respond(ctx, ports.RawNativeCallbackResponse{StatusCode: 200, ContentType: "text/plain", Body: []byte("ok")})
		require.Error(t, err)
		require.Equal(t, ports.EffectRefused, result)
		return err
	})})
	require.NoError(t, err)
	req := httptest.NewRequest("POST", binding.Route, strings.NewReader("oversize"))
	req.Header.Set("Authorization", "Native "+secret)
	w := httptest.NewRecorder()
	c.ServeHTTP(w, req)
	require.Equal(t, 400, w.Code)
	require.Zero(t, calls)
	req = httptest.NewRequest("POST", binding.Route, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Native "+secret)
	w = httptest.NewRecorder()
	c.ServeHTTP(w, req)
	require.Equal(t, 503, w.Code)
	require.Equal(t, 1, calls)
}
