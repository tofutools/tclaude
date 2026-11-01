package agentd_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/pwf"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const popupCap = 8 // maxPendingApprovalsPerCaller

// A caller past the cap is told so, and the requests already waiting are not
// dropped: each still gets the human's answer. Another caller is not affected.
func TestAskHumanRefusesPastTheCapAndKeepsThoseWaiting(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	const conv, other = "popup-cap-aaaa-bbbb-000000000001", "popup-cap-aaaa-bbbb-000000000002"
	for i, c := range []string{conv, other} {
		f.HaveAliveSession(c, "popup-cap", "tmux-popup-cap-"+string(rune('a'+i)), f.TestCwd("popup"))
		_, _, err := db.EnsureAgentForConv(c, "test")
		require.NoError(t, err)
	}
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	raised, answer, restore := agentd.StubWaitingApprovalForTest()
	t.Cleanup(restore)
	file := pwfFile(t)
	ask := func(caller string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/process/import", bytes.NewReader(file))
		req.Header.Set("Content-Type", pwf.MediaType)
		req.Header.Set("X-Tclaude-Ask-Human", "30s")
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(req, caller))
	}

	var waiting sync.WaitGroup
	codes := make([]int, popupCap+1)
	for i := range codes {
		caller := conv
		if i == popupCap {
			caller = other
		}
		waiting.Go(func() { codes[i] = ask(caller).Code })
	}
	for range codes {
		<-raised
	}

	refused := ask(conv)
	assert.Equal(t, http.StatusTooManyRequests, refused.Code, refused.Body.String())
	assert.Contains(t, refused.Body.String(), "approval_queue_full")
	assert.Contains(t, refused.Body.String(), "already waiting for the human")
	assert.NotEmpty(t, refused.Header().Get("Retry-After"))

	answer()
	waiting.Wait()
	for _, code := range codes {
		assert.Equal(t, http.StatusOK, code, "a request already waiting keeps its popup")
	}
	assert.Equal(t, http.StatusOK, ask(conv).Code, "answered requests give their slots back")
}

// A body read to describe it counts against what waiting requests may hold, and
// the popup is not raised for one past that. The slot and bytes come back.
func TestAskHumanRefusesABodyTooLargeToHold(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	const conv = "popup-body-aaaa-bbbb-000000000001"
	f.HaveAliveSession(conv, "popup-body", "tmux-popup-body", f.TestCwd("popup"))
	_, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	popups, restore := agentd.StubCapturingApprovalForTest(true)
	t.Cleanup(restore)
	file := pwfFile(t)
	ask := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/process/import", bytes.NewReader(file))
		req.Header.Set("Content-Type", pwf.MediaType)
		req.Header.Set("X-Tclaude-Ask-Human", "30s")
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(req, conv))
	}

	lower := agentd.SetApprovalHeldBytesForTest(int64(len(file)) - 1)
	refused := ask()
	lower()
	assert.Equal(t, http.StatusTooManyRequests, refused.Code, refused.Body.String())
	assert.Contains(t, refused.Body.String(), "approval_body_too_large")
	assert.Empty(t, *popups, "no popup for a request that could not be held")

	assert.Equal(t, http.StatusOK, ask().Code)
	assert.Len(t, *popups, 1)
}
