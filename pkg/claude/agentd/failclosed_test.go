package agentd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClassify exercises the single classification chokepoint across
// every caller class and the load-bearing precedence rules.
func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		p    peer
		want callerClass
	}{
		{"pid 0 -> unidentified", peer{PID: 0}, classUnidentified},
		{"cc ancestor + conv-id -> agent", peer{PID: 9, HasClaudeAncestor: true, ConvID: "c1"}, classAgent},
		{"cc ancestor, no conv-id -> agentUnknown", peer{PID: 9, HasClaudeAncestor: true}, classAgentUnknown},
		{"no ancestor + valid token -> human", peer{PID: 9, HumanTokenValid: true}, classHuman},
		{"no ancestor, no token -> unconfirmed", peer{PID: 9}, classUnconfirmed},
		{"dashboard human -> human", peer{PID: 1, DashboardHuman: true}, classHuman},
		// Precedence — load-bearing: a Claude Code ancestor wins over an
		// inherited operator token. An agent that picked up
		// TCLAUDE_HUMAN_TOKEN from the human's shell must NOT escalate.
		{"agent + token -> still agent", peer{PID: 9, HasClaudeAncestor: true, ConvID: "c1", HumanTokenValid: true}, classAgent},
		{"agentUnknown + token -> still agentUnknown", peer{PID: 9, HasClaudeAncestor: true, HumanTokenValid: true}, classAgentUnknown},
		// DashboardHuman is checked first, so it holds even for the
		// synthetic peer's degenerate PID.
		{"dashboard human wins over pid 0", peer{DashboardHuman: true}, classHuman},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.p
			assert.Equal(t, tc.want, classify(&p))
		})
	}
}

// withTokenRestore snapshots the package operator token and restores it
// after the test, so token-mutating tests don't bleed into each other.
func withTokenRestore(t *testing.T) {
	t.Helper()
	prev := currentOperatorToken()
	t.Cleanup(func() {
		operatorTokenMu.Lock()
		operatorToken = prev
		operatorTokenMu.Unlock()
	})
}

// TestVerifyHumanToken covers the operator-token header check.
func TestVerifyHumanToken(t *testing.T) {
	withTokenRestore(t)
	tok := generateOperatorToken()
	require.True(t, strings.HasPrefix(tok, humanTokenPrefix), "token carries the tclo_ prefix")

	mk := func(hdr string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if hdr != "" {
			r.Header.Set(humanTokenHeader, hdr)
		}
		return r
	}
	assert.True(t, verifyHumanToken(mk(tok)), "correct token verifies")
	assert.False(t, verifyHumanToken(mk("tclo_wrong")), "wrong token rejected")
	assert.False(t, verifyHumanToken(mk("")), "absent header rejected")

	// With no token generated, nothing verifies — even a non-empty header.
	operatorTokenMu.Lock()
	operatorToken = ""
	operatorTokenMu.Unlock()
	assert.False(t, verifyHumanToken(mk(tok)), "no token generated => nothing verifies")
}

// TestHandleAuthToken covers the bootstrap endpoint: it hands the token
// to a human (no Claude Code ancestor) and refuses an agent caller.
func TestHandleAuthToken(t *testing.T) {
	withTokenRestore(t)
	tok := generateOperatorToken()

	// Human — no Claude Code ancestor — gets the token.
	w := httptest.NewRecorder()
	handleAuthToken(w, requestWithPeer(&peer{PID: 7}))
	require.Equal(t, http.StatusOK, w.Code, "human gets the token; body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), tok)

	// An agent (Claude Code ancestor) is refused — it cannot bootstrap a
	// token, so a sandboxed agent can never obtain one.
	w = httptest.NewRecorder()
	handleAuthToken(w, requestWithPeer(&peer{PID: 7, HasClaudeAncestor: true, ConvID: "c1"}))
	assert.Equal(t, http.StatusForbidden, w.Code, "agent is refused the token")

	// An unreadable peer PID is refused 401.
	w = httptest.NewRecorder()
	handleAuthToken(w, requestWithPeer(&peer{PID: 0}))
	assert.Equal(t, http.StatusUnauthorized, w.Code, "pid 0 is refused")
}

// TestAsDashboardHumanPeer_ClassifiesAsHuman guards the gap-2 fix: the
// cookie-authenticated dashboard delegation must classify as the human
// (it legitimately holds no operator token) — not as classUnconfirmed.
func TestAsDashboardHumanPeer_ClassifiesAsHuman(t *testing.T) {
	r := asDashboardHumanPeer(httptest.NewRequest(http.MethodPost, "/api/groups", nil))
	assert.Equal(t, classHuman, classify(peerFromContext(r.Context())))
}

// TestAuthCronWrite_FailsClosedForUnconfirmed guards the gap-1 fix: a
// caller that is neither a confirmed agent nor the human is refused at
// authCronWrite (a previously-inline `!HasClaudeAncestor` site) rather
// than silently treated as the human.
func TestAuthCronWrite_FailsClosedForUnconfirmed(t *testing.T) {
	w := httptest.NewRecorder()
	_, ok := authCronWrite(w, requestWithPeer(&peer{PID: 7}), "target-conv")
	assert.False(t, ok, "unconfirmed caller must be refused")
	assert.Equal(t, http.StatusForbidden, w.Code)

	// The human operator still passes.
	w = httptest.NewRecorder()
	_, ok = authCronWrite(w, requestWithPeer(&peer{PID: 7, HumanTokenValid: true}), "target-conv")
	assert.True(t, ok, "human operator passes; body=%s", w.Body.String())
}

// TestFailClosed_EndToEndAtHumanEndpoint drives callers through the
// production /v1 mux at a human-only endpoint (group create): an
// unconfirmed caller is refused 403; the human operator succeeds.
func TestFailClosed_EndToEndAtHumanEndpoint(t *testing.T) {
	setupTestDB(t)
	h := BuildHandlerForTest()

	newReq := func(stamp func(*http.Request) *http.Request) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/groups", strings.NewReader(`{"name":"g1"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, stamp(r))
		return w
	}

	w := newReq(AsUnconfirmedPeer)
	assert.Equal(t, http.StatusForbidden, w.Code, "unconfirmed caller refused; body=%s", w.Body.String())

	w = newReq(AsHumanPeer)
	assert.Less(t, w.Code, 400, "human operator creates the group; body=%s", w.Body.String())
}
