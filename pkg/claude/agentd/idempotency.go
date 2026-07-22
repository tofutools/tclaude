package agentd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	idempotencyTTL          = 30 * time.Minute
	idempotencyPollInterval = 25 * time.Millisecond
	idempotencyReplayHeader = "X-Tclaude-Idempotent-Replay"
)

var idempotencyOwnerID = uuid.NewString()

func idempotencyRequests(h http.Handler) http.Handler {
	return idempotencyRequestsWithOwner(h, idempotencyOwnerID)
}

// idempotencyRequestsWithOwner gives every mutating CLI request a durable
// execution boundary. Completed responses are replayed across connection cuts
// and daemon restarts. A pending row owned by another daemon is deliberately
// reported as ambiguous: generic middleware cannot know whether the old
// process committed an endpoint-specific side effect before it died.
func idempotencyRequestsWithOwner(h http.Handler, ownerID string) http.Handler {
	return idempotencyRequestsWithOwnerAndWaitHook(h, ownerID, nil)
}

func idempotencyRequestsWithOwnerAndWaitHook(h http.Handler, ownerID string, waitHook func()) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get(agent.IdempotencyKeyHeader))
		if key == "" || !isMutatingMethod(r.Method) {
			h.ServeHTTP(w, r)
			return
		}
		if parsed, err := uuid.Parse(key); err != nil || parsed.String() != strings.ToLower(key) {
			writeError(w, http.StatusBadRequest, "invalid_arg", "invalid "+agent.IdempotencyKeyHeader)
			return
		}

		digest := strings.TrimSpace(r.Header.Get(agent.RequestDigestHeader))
		if decoded, err := hex.DecodeString(digest); err != nil || len(decoded) != sha256.Size {
			writeError(w, http.StatusBadRequest, "invalid_arg", "invalid "+agent.RequestDigestHeader)
			return
		}
		fingerprint := idempotencyFingerprint(r, digest)

		now := time.Now()
		record, claimed, err := db.ClaimAgentdRequest(key, fingerprint, ownerID, now, now.Add(idempotencyTTL))
		if err != nil {
			writeIdempotencyUnknown(w,
				"the request record could not be inspected; the operation's outcome is unknown and the mutation was not retried: "+err.Error())
			return
		}
		if claimed {
			executeIdempotentRequest(w, r, h, key, ownerID)
			return
		}
		if record.Fingerprint != fingerprint {
			writeError(w, http.StatusConflict, "idempotency_conflict",
				"request ID was already used for a different operation")
			return
		}
		if record.State == db.IdempotencyCompleted {
			replayIdempotentResponse(w, record)
			return
		}
		if record.OwnerID != ownerID {
			writeIdempotencyUnknown(w,
				"the previous agentd stopped while this operation was pending; its outcome is unknown and the mutation was not retried")
			return
		}

		if waitHook != nil {
			waitHook()
		}
		record, err = waitForIdempotentResponse(r.Context(), key, ownerID)
		switch {
		case err == nil:
			replayIdempotentResponse(w, record)
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			return
		default:
			writeIdempotencyUnknown(w,
				"the pending request could not be observed to completion; its outcome is unknown and the mutation was not retried: "+err.Error())
		}
	})
}

func writeIdempotencyUnknown(w http.ResponseWriter, message string) {
	writeError(w, http.StatusConflict, "idempotency_unknown", message)
}

func idempotencyFingerprint(r *http.Request, requestDigest string) string {
	p := peerFromContext(r.Context())
	scope := fmt.Sprintf("class=%d;conv=%s;pid=%d", classify(p), p.ConvID, p.PID)
	h := sha256.New()
	_, _ = io.WriteString(h, scope)
	_, _ = io.WriteString(h, "\x00"+requestDigest)
	return hex.EncodeToString(h.Sum(nil))
}

func executeIdempotentRequest(w http.ResponseWriter, r *http.Request, h http.Handler, key, ownerID string) {
	rec := newBufferedResponse()
	h.ServeHTTP(rec, r)
	headersJSON, err := json.Marshal(rec.header)
	if err != nil {
		writeIdempotencyUnknown(w,
			"the operation ran, but its response could not be encoded; its outcome is unknown and the mutation was not retried: "+err.Error())
		return
	}
	if err := db.CompleteAgentdRequest(key, ownerID, rec.status, string(headersJSON), rec.body.Bytes()); err != nil {
		// The endpoint has already run. If its response cannot be made durable,
		// callers must not treat this as an ordinary failed request and retry the
		// mutation under a fresh key: the side effect may have committed.
		writeIdempotencyUnknown(w,
			"the operation ran, but its response could not be recorded; its outcome is unknown and the mutation was not retried: "+err.Error())
		return
	}
	writeBufferedResponse(w, rec)
}

func waitForIdempotentResponse(ctx context.Context, key, ownerID string) (db.AgentdIdempotencyRecord, error) {
	ticker := time.NewTicker(idempotencyPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return db.AgentdIdempotencyRecord{}, ctx.Err()
		case <-ticker.C:
			record, err := db.GetAgentdRequest(key)
			if err != nil {
				return db.AgentdIdempotencyRecord{}, err
			}
			if record.State == db.IdempotencyCompleted {
				return record, nil
			}
			if record.OwnerID != ownerID {
				return db.AgentdIdempotencyRecord{}, fmt.Errorf("idempotency owner changed while waiting")
			}
		}
	}
}

func replayIdempotentResponse(w http.ResponseWriter, record db.AgentdIdempotencyRecord) {
	var headers http.Header
	if err := json.Unmarshal([]byte(record.HeadersJSON), &headers); err != nil {
		writeIdempotencyUnknown(w,
			"the completed response could not be reconstructed; the operation's outcome is unknown and the mutation was not retried: "+err.Error())
		return
	}
	for key, values := range headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set(idempotencyReplayHeader, "true")
	w.WriteHeader(record.Status)
	_, _ = w.Write(record.ResponseBody)
}

type bufferedResponse struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func newBufferedResponse() *bufferedResponse {
	return &bufferedResponse{header: make(http.Header), status: http.StatusOK}
}

func (r *bufferedResponse) Header() http.Header { return r.header }

func (r *bufferedResponse) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
}

func (r *bufferedResponse) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(body)
}

func writeBufferedResponse(w http.ResponseWriter, rec *bufferedResponse) {
	for key, values := range rec.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(rec.status)
	_, _ = w.Write(rec.body.Bytes())
}
