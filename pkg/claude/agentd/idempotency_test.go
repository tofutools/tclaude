package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func idempotencyRequest(t *testing.T, key, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/test?mode=one", strings.NewReader(body))
	req.Header.Set(agent.IdempotencyKeyHeader, key)
	digest := sha256.Sum256([]byte(req.Method + "\x00" + req.URL.RequestURI() + "\x00" + body))
	req.Header.Set(agent.RequestDigestHeader, fmt.Sprintf("%x", digest))
	p := &peer{PID: 123, ConvID: "conv-idempotency", HasClaudeAncestor: true}
	return req.WithContext(context.WithValue(req.Context(), peerKey{}, p))
}

func TestIdempotencyCompletedResponseReplaysAcrossDaemonOwners(t *testing.T) {
	setupTestDB(t)
	key := uuid.NewString()
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("X-Result", "created")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":42}`))
	})

	first := httptest.NewRecorder()
	idempotencyRequestsWithOwner(handler, "daemon-a").ServeHTTP(first, idempotencyRequest(t, key, "payload"))
	require.Equal(t, http.StatusCreated, first.Code)

	replay := httptest.NewRecorder()
	idempotencyRequestsWithOwner(handler, "daemon-b").ServeHTTP(replay, idempotencyRequest(t, key, "payload"))

	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, http.StatusCreated, replay.Code)
	assert.Equal(t, first.Body.String(), replay.Body.String())
	assert.Equal(t, "created", replay.Header().Get("X-Result"))
	assert.Equal(t, "true", replay.Header().Get(idempotencyReplayHeader))
}

func TestIdempotencyPendingFromReplacedDaemonReportsUnknown(t *testing.T) {
	setupTestDB(t)
	key := uuid.NewString()
	req := idempotencyRequest(t, key, "payload")
	now := time.Now()
	_, claimed, err := db.ClaimAgentdRequest(key, idempotencyFingerprint(req, req.Header.Get(agent.RequestDigestHeader)),
		"old-daemon", now, now.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)

	var calls atomic.Int32
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) })
	rec := httptest.NewRecorder()
	idempotencyRequestsWithOwner(handler, "new-daemon").ServeHTTP(rec, idempotencyRequest(t, key, "payload"))

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Zero(t, calls.Load())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "idempotency_unknown", body["code"])
	assert.Contains(t, body["error"], "outcome is unknown")
}

func TestIdempotency5xxIsReplayedWithoutRerunningMutation(t *testing.T) {
	setupTestDB(t)
	key := uuid.NewString()
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "failed after mutation", http.StatusServiceUnavailable)
	})
	middleware := idempotencyRequestsWithOwner(handler, "daemon-a")

	first := httptest.NewRecorder()
	middleware.ServeHTTP(first, idempotencyRequest(t, key, "payload"))
	assert.Equal(t, http.StatusServiceUnavailable, first.Code)

	second := httptest.NewRecorder()
	middleware.ServeHTTP(second, idempotencyRequest(t, key, "payload"))
	assert.Equal(t, http.StatusServiceUnavailable, second.Code)
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, first.Body.String(), second.Body.String())
	assert.Equal(t, "true", second.Header().Get(idempotencyReplayHeader))
}

func TestIdempotencyConcurrentRetryWaitsForOriginal(t *testing.T) {
	setupTestDB(t)
	_, err := db.Open()
	require.NoError(t, err)
	key := uuid.NewString()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		close(started)
		<-release
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})
	retryWaiting := make(chan struct{})
	middleware := idempotencyRequestsWithOwnerAndWaitHook(handler, "daemon-a", func() {
		close(retryWaiting)
	})

	first := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		middleware.ServeHTTP(first, idempotencyRequest(t, key, "payload"))
		close(firstDone)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("original handler did not start")
	}

	second := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		middleware.ServeHTTP(second, idempotencyRequest(t, key, "payload"))
		close(secondDone)
	}()
	select {
	case <-retryWaiting:
	case <-time.After(2 * time.Second):
		t.Fatal("retry did not reach the pending-request wait path")
	}
	assert.Equal(t, int32(1), calls.Load(), "retry must not execute the handler")
	close(release)

	for name, done := range map[string]<-chan struct{}{"first": firstDone, "second": secondDone} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s request did not finish", name)
		}
	}
	assert.Equal(t, http.StatusCreated, first.Code)
	assert.Equal(t, http.StatusCreated, second.Code)
	assert.Equal(t, first.Body.String(), second.Body.String())
	assert.Equal(t, "true", second.Header().Get(idempotencyReplayHeader))
}

func TestIdempotencyRejectsRequestIDReuseWithDifferentPayload(t *testing.T) {
	setupTestDB(t)
	key := uuid.NewString()
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	middleware := idempotencyRequestsWithOwner(handler, "daemon-a")

	middleware.ServeHTTP(httptest.NewRecorder(), idempotencyRequest(t, key, "first"))
	conflict := httptest.NewRecorder()
	middleware.ServeHTTP(conflict, idempotencyRequest(t, key, "different"))

	assert.Equal(t, http.StatusConflict, conflict.Code)
	assert.Equal(t, int32(1), calls.Load())
}
