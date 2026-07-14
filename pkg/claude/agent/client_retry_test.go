package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type transportResult struct {
	status int
	err    error
}

func scriptedClient(t *testing.T, results []transportResult, bodies *[][]byte) (*http.Client, *int) {
	t.Helper()
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if bodies != nil && req.Body != nil {
			body, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			*bodies = append(*bodies, body)
		}
		require.Less(t, attempts, len(results), "unexpected extra request")
		result := results[attempts]
		attempts++
		if result.err != nil {
			return nil, result.err
		}
		return &http.Response{
			StatusCode: result.status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(http.StatusText(result.status))),
			Request:    req,
		}, nil
	})}
	return client, &attempts
}

func instantRetryPolicy(connection, server []time.Duration, slept *[]time.Duration) daemonRetryPolicy {
	return daemonRetryPolicy{
		connectionBackoffs: connection,
		serverBackoffs:     server,
		retryMutations:     true,
		sleep: func(_ context.Context, delay time.Duration) error {
			*slept = append(*slept, delay)
			return nil
		},
	}
}

func TestDoDaemonRequestStopsAfterTwo5xxRetries(t *testing.T) {
	results := []transportResult{{status: 503}, {status: 502}, {status: 500}}
	client, attempts := scriptedClient(t, results, nil)
	req, err := http.NewRequest(http.MethodGet, "http://_/v1/peers", nil)
	require.NoError(t, err)
	var stderr bytes.Buffer
	var slept []time.Duration

	resp, err := doDaemonRequest(client, req, &stderr,
		instantRetryPolicy([]time.Duration{time.Second}, []time.Duration{time.Second, 2 * time.Second}, &slept))
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, 3, *attempts)
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second}, slept)
	assert.Contains(t, stderr.String(), "agentd returned HTTP 503; retrying in 1s (1/2)")
	assert.Contains(t, stderr.String(), "agentd returned HTTP 502; retrying in 2s (2/2)")
}

func TestDoDaemonRequestUsesIndependent5xxAndConnectionBudgets(t *testing.T) {
	connectionErr := &net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED}
	results := []transportResult{
		{status: 503},
		{status: 503},
		{err: connectionErr},
		{err: connectionErr},
		{status: 200},
	}
	client, attempts := scriptedClient(t, results, nil)
	req, err := http.NewRequest(http.MethodGet, "http://_/v1/messages", nil)
	require.NoError(t, err)
	var stderr bytes.Buffer
	var slept []time.Duration
	policy := instantRetryPolicy(
		[]time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
		[]time.Duration{time.Second, 2 * time.Second},
		&slept,
	)

	resp, err := doDaemonRequest(client, req, &stderr, policy)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 5, *attempts)
	assert.Equal(t,
		[]time.Duration{time.Second, 2 * time.Second, time.Second, 2 * time.Second}, slept)
	assert.Contains(t, stderr.String(), "agentd connection failed:")
	assert.Contains(t, stderr.String(), "retrying in 2s (2/3)")
}

func TestDoDaemonRequestExhaustsConnectionBackoff(t *testing.T) {
	connectionErr := &net.OpError{Op: "read", Net: "unix", Err: syscall.ECONNRESET}
	results := make([]transportResult, 6)
	for i := range results {
		results[i].err = connectionErr
	}
	client, attempts := scriptedClient(t, results, nil)
	req, err := http.NewRequest(http.MethodGet, "http://_/v1/whoami", nil)
	require.NoError(t, err)
	var stderr bytes.Buffer
	var slept []time.Duration
	backoffs := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}

	resp, err := doDaemonRequest(client, req, &stderr,
		instantRetryPolicy(backoffs, []time.Duration{time.Second}, &slept))

	assert.Nil(t, resp)
	assert.ErrorIs(t, err, syscall.ECONNRESET)
	assert.Equal(t, 6, *attempts)
	assert.Equal(t, backoffs, slept)
	assert.Contains(t, stderr.String(), "retrying in 16s (5/5)")
}

func TestDoDaemonRequestDoesNotRetry4xxOrNonConnectionErrors(t *testing.T) {
	t.Run("4xx", func(t *testing.T) {
		client, attempts := scriptedClient(t, []transportResult{{status: 429}}, nil)
		req, err := http.NewRequest(http.MethodGet, "http://_/v1/peers", nil)
		require.NoError(t, err)
		var slept []time.Duration

		resp, err := doDaemonRequest(client, req, io.Discard,
			instantRetryPolicy([]time.Duration{time.Second}, []time.Duration{time.Second}, &slept))
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
		assert.Equal(t, 1, *attempts)
		assert.Empty(t, slept)
	})

	t.Run("non-connection error", func(t *testing.T) {
		client, attempts := scriptedClient(t, []transportResult{{err: errors.New("bad protocol")}}, nil)
		req, err := http.NewRequest(http.MethodGet, "http://_/v1/peers", nil)
		require.NoError(t, err)
		var slept []time.Duration

		resp, err := doDaemonRequest(client, req, io.Discard,
			instantRetryPolicy([]time.Duration{time.Second}, []time.Duration{time.Second}, &slept))
		assert.Nil(t, resp)
		assert.EqualError(t, err, "Get \"http://_/v1/peers\": bad protocol")
		assert.Equal(t, 1, *attempts)
		assert.Empty(t, slept)
	})
}

func TestWaitForDaemonUsesFullConnectionBackoff(t *testing.T) {
	checks := 0
	available := func() bool {
		checks++
		return checks == 4
	}
	var stderr bytes.Buffer
	var slept []time.Duration
	backoffs := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	policy := instantRetryPolicy(backoffs, nil, &slept)

	assert.True(t, waitForDaemon(&stderr, available, policy))
	assert.Equal(t, 4, checks)
	assert.Equal(t, backoffs, slept)
	assert.Contains(t, stderr.String(), "agentd is unavailable; retrying in 4s (3/3)")
}

func TestIsConnectionFailure(t *testing.T) {
	assert.True(t, isConnectionFailure(io.EOF))
	assert.True(t, isConnectionFailure(&net.OpError{Op: "dial", Net: "unix", Err: syscall.ENOENT}))
	assert.False(t, isConnectionFailure(context.DeadlineExceeded))
	assert.False(t, isConnectionFailure(context.Canceled))
	assert.False(t, isConnectionFailure(errors.New("bad protocol")))
}

func TestDoDaemonRequestKeepsOneIdempotencyKeyAcrossRetries(t *testing.T) {
	var keys []string
	var digests []string
	attempt := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		keys = append(keys, req.Header.Get(IdempotencyKeyHeader))
		digests = append(digests, req.Header.Get(RequestDigestHeader))
		attempt++
		if attempt == 1 {
			return nil, &net.OpError{Op: "write", Net: "unix", Err: syscall.EPIPE}
		}
		return &http.Response{StatusCode: http.StatusCreated, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("response")), Request: req}, nil
	})}
	req, err := http.NewRequest(http.MethodPost, "http://_/v1/messages", strings.NewReader("payload"))
	require.NoError(t, err)
	var slept []time.Duration

	resp, err := doDaemonRequest(client, req, io.Discard,
		instantRetryPolicy([]time.Duration{time.Second}, nil, &slept))
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Len(t, keys, 2)
	assert.NotEmpty(t, keys[0])
	assert.Equal(t, keys[0], keys[1])
	require.Len(t, digests, 2)
	assert.Len(t, digests[0], sha256.Size*2)
	assert.Equal(t, digests[0], digests[1])
}

func TestAttachIdempotencyKeySkipsReads(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://_/v1/peers", nil)
	require.NoError(t, err)
	require.NoError(t, attachIdempotencyKey(req))
	assert.Empty(t, req.Header.Get(IdempotencyKeyHeader))
}

func TestDoDaemonRequestDoesNotRetryMutationWithoutServerSupport(t *testing.T) {
	client, attempts := scriptedClient(t, []transportResult{{status: 503}}, nil)
	req, err := http.NewRequest(http.MethodPost, "http://_/v1/messages", strings.NewReader("payload"))
	require.NoError(t, err)
	var slept []time.Duration
	policy := instantRetryPolicy([]time.Duration{time.Second}, []time.Duration{time.Second}, &slept)
	policy.retryMutations = false

	resp, err := doDaemonRequest(client, req, io.Discard, policy)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, 1, *attempts)
	assert.Empty(t, slept)
}

func TestDoDaemonRequestDoesNotRetryMutation5xx(t *testing.T) {
	client, attempts := scriptedClient(t, []transportResult{{status: 503}}, nil)
	req, err := http.NewRequest(http.MethodPost, "http://_/v1/messages", strings.NewReader("payload"))
	require.NoError(t, err)
	var slept []time.Duration

	resp, err := doDaemonRequest(client, req, io.Discard,
		instantRetryPolicy([]time.Duration{time.Second}, []time.Duration{time.Second}, &slept))
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, 1, *attempts)
	assert.Empty(t, slept)
}

type truncatedReader struct {
	data []byte
	done bool
}

func (r *truncatedReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.ErrUnexpectedEOF
	}
	r.done = true
	return copy(p, r.data), io.ErrUnexpectedEOF
}

func TestDoDaemonRequestRetriesTruncatedResponseBody(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		body := io.ReadCloser(io.NopCloser(strings.NewReader("complete")))
		if attempts == 1 {
			body = io.NopCloser(&truncatedReader{data: []byte("partial")})
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: req}, nil
	})}
	req, err := http.NewRequest(http.MethodGet, "http://_/v1/peers", nil)
	require.NoError(t, err)
	var stderr bytes.Buffer
	var slept []time.Duration

	resp, err := doDaemonRequest(client, req, &stderr,
		instantRetryPolicy([]time.Duration{time.Second}, nil, &slept))
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, "complete", string(body))
	assert.Equal(t, 2, attempts)
	assert.Equal(t, []time.Duration{time.Second}, slept)
	assert.Contains(t, stderr.String(), "connection failed while reading response")
}

func TestDaemonResponseBytesReusesBufferedResponse(t *testing.T) {
	original := []byte("download")
	resp := &http.Response{Body: newBufferedDaemonBody(original)}

	got, err := daemonResponseBytes(resp)
	require.NoError(t, err)
	require.NotEmpty(t, got)

	got[0] = 'D'
	assert.Equal(t, byte('D'), original[0])
}

func TestDaemonSupportsIdempotency(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "current daemon", body: `{"popup_base_url":"http://127.0.0.1","idempotency":"v1"}`, want: true},
		{name: "older daemon", body: `{"popup_base_url":"http://127.0.0.1"}`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				assert.Equal(t, "/v1/info", req.URL.Path)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(tt.body)), Request: req}, nil
			})}

			got, err := daemonSupportsIdempotency(client, io.Discard)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
