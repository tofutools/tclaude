package agentd_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// clipRecorder captures every text handleClipboard hands to the platform
// clipboard-write seam, and can inject a failure to drive the copy-tool
// error branch. Installed via agentd.SetClipboardWriterForTest.
type clipRecorder struct {
	texts []string
	err   error
}

func (c *clipRecorder) install(t *testing.T) {
	t.Helper()
	t.Cleanup(agentd.SetClipboardWriterForTest(func(text string) error {
		c.texts = append(c.texts, text)
		return c.err
	}))
}

// Scenario: an agent holding the human.clipboard slug copies text. The
// daemon gates on the slug, then hands the EXACT bytes to the platform
// writer (whitespace preserved) and reports the byte count.
func TestClipboard_GrantedCopies(t *testing.T) {
	f := newFlow(t)
	var rec clipRecorder
	rec.install(t)

	const conv = "clp0-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "worker")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanClipboard, "test"))

	const payload = "  git rebase -i main\n" // leading spaces + trailing newline, on purpose
	r := testharness.JSONRequest(t, http.MethodPost, "/v1/clipboard",
		map[string]any{"text": payload})
	r = agentd.AsAgentPeer(r, conv)
	resp := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body.String())

	require.Len(t, rec.texts, 1, "the copy path should run exactly once")
	assert.Equal(t, payload, rec.texts[0], "text must reach the writer verbatim")
	assert.Contains(t, resp.Body.String(), fmt.Sprintf("\"bytes\":%d", len(payload)))
	assert.Contains(t, resp.Body.String(), "\"copied\":true")
}

// Scenario: an agent with no grant and no --ask-human header is refused
// with 403 naming the slug, and nothing is copied. This is the anti-abuse
// baseline — the clipboard is the human's real machine surface.
func TestClipboard_UngrantedForbidden(t *testing.T) {
	f := newFlow(t)
	var rec clipRecorder
	rec.install(t)

	const conv = "clp1-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "worker")

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/clipboard",
		map[string]any{"text": "let me paste into your clipboard"})
	r = agentd.AsAgentPeer(r, conv)
	resp := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusForbidden, resp.Code, "body=%s", resp.Body.String())
	assert.Contains(t, resp.Body.String(), agentd.PermHumanClipboard, "the 403 names the missing slug")
	assert.Empty(t, rec.texts, "a denied caller must not reach the clipboard")
}

// Scenario: a group OWNER with no slug is still refused — unlike
// human.notify, human.clipboard is NOT owner-implied. Owning a group does
// not grant a path to the human's clipboard; only an explicit grant or a
// popup approval does. Pins the "lean no" design decision.
func TestClipboard_GroupOwnerStillForbidden(t *testing.T) {
	f := newFlow(t)
	var rec clipRecorder
	rec.install(t)

	const ownerConv = "clp2-1111-2222-3333-4444"
	g := f.HaveGroup("owned-team")
	f.HaveMember("owned-team", ownerConv)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, ownerConv, "test"))

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/clipboard",
		map[string]any{"text": "owner without the slug"})
	r = agentd.AsAgentPeer(r, ownerConv)
	resp := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusForbidden, resp.Code,
		"group ownership must NOT confer clipboard access; body=%s", resp.Body.String())
	assert.Empty(t, rec.texts, "an owner without the slug must not reach the clipboard")
}

// Scenario: no slug, but the caller adds X-Tclaude-Ask-Human and the popup
// is stubbed to APPROVE — the copy runs. The --ask-human escape hatch is
// the one-off-copy UX from the ticket.
func TestClipboard_AskHumanApproveCopies(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.StubApprovalForTest(true))

	f := newFlow(t)
	var rec clipRecorder
	rec.install(t)

	const conv = "clp3-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "worker")

	const payload = "one-off copy"
	r := testharness.JSONRequest(t, http.MethodPost, "/v1/clipboard",
		map[string]any{"text": payload})
	r.Header.Set("X-Tclaude-Ask-Human", "30s")
	r = agentd.AsAgentPeer(r, conv)
	resp := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body.String())
	require.Len(t, rec.texts, 1)
	assert.Equal(t, payload, rec.texts[0])
}

// Scenario: same, but the popup DENIES — 403 and nothing is copied. The
// popup is an escape hatch, not a free pass.
func TestClipboard_AskHumanDenyRefuses(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.StubApprovalForTest(false))

	f := newFlow(t)
	var rec clipRecorder
	rec.install(t)

	const conv = "clp4-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "worker")

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/clipboard",
		map[string]any{"text": "should not land"})
	r.Header.Set("X-Tclaude-Ask-Human", "30s")
	r = agentd.AsAgentPeer(r, conv)
	resp := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusForbidden, resp.Code, "body=%s", resp.Body.String())
	assert.Empty(t, rec.texts, "a popup-denied copy must not reach the clipboard")
}

// Scenario: the human (no harness ancestor) is implicitly allowed — same
// convention as every other endpoint.
func TestClipboard_HumanBypasses(t *testing.T) {
	f := newFlow(t)
	var rec clipRecorder
	rec.install(t)

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/clipboard",
		map[string]any{"text": "human-initiated copy"})
	r = agentd.AsHumanPeer(r)
	resp := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Body.String())
	require.Len(t, rec.texts, 1)
	assert.Equal(t, "human-initiated copy", rec.texts[0])
}

// Scenario: the platform copy tool fails (no display, tool missing). The
// grant passed, but the write errored — the daemon surfaces a 500 rather
// than a false success.
func TestClipboard_WriterFailureSurfaces(t *testing.T) {
	f := newFlow(t)
	rec := clipRecorder{err: fmt.Errorf("no clipboard tool found on PATH")}
	rec.install(t)

	const conv = "clp5-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "worker")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanClipboard, "test"))

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/clipboard",
		map[string]any{"text": "will fail to copy"})
	r = agentd.AsAgentPeer(r, conv)
	resp := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusInternalServerError, resp.Code, "body=%s", resp.Body.String())
	assert.Contains(t, resp.Body.String(), "clipboard")
}

// Scenario: an empty text field is a 400 — copying nothing is a client
// error, and the daemon rejects it before the writer.
func TestClipboard_EmptyTextRejected(t *testing.T) {
	f := newFlow(t)
	var rec clipRecorder
	rec.install(t)

	const conv = "clp6-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "worker")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanClipboard, "test"))

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/clipboard",
		map[string]any{"text": ""})
	r = agentd.AsAgentPeer(r, conv)
	resp := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusBadRequest, resp.Code, "body=%s", resp.Body.String())
	assert.Empty(t, rec.texts)
}

// Scenario: a decoded payload past maxClipboardBytes is rejected 400 (the
// post-decode cap), after the wire-byte guard lets it through. Nothing is
// copied.
func TestClipboard_OversizedTextRejected(t *testing.T) {
	f := newFlow(t)
	var rec clipRecorder
	rec.install(t)

	const conv = "clp7-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "worker")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanClipboard, "test"))

	// 256 KiB + 1 — over the decoded cap, under the ~1.5 MiB wire cap.
	huge := strings.Repeat("x", 256*1024+1)
	r := testharness.JSONRequest(t, http.MethodPost, "/v1/clipboard",
		map[string]any{"text": huge})
	r = agentd.AsAgentPeer(r, conv)
	resp := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusBadRequest, resp.Code, "body=%s", resp.Body.String())
	assert.Contains(t, resp.Body.String(), "too long")
	assert.Empty(t, rec.texts)
}

// Scenario (regression): a payload LARGER than the popup preview cap
// (64 KiB) approved via --ask-human is still copied IN FULL. The popup's
// body snapshot used to restore only 64 KiB, so a big clipboard copy
// decoded from a truncated body and 400'd *after* the human approved — an
// approve-then-fail. The snapshot now preserves the whole body, so the
// approved copy lands intact.
func TestClipboard_AskHumanLargePayloadCopiesInFull(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.StubApprovalForTest(true))

	f := newFlow(t)
	var rec clipRecorder
	rec.install(t)

	const conv = "clp8-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "worker")

	// 100 KiB — comfortably over the 64 KiB preview cap, under the 256 KiB
	// decoded cap.
	payload := strings.Repeat("z", 100*1024)
	r := testharness.JSONRequest(t, http.MethodPost, "/v1/clipboard",
		map[string]any{"text": payload})
	r.Header.Set("X-Tclaude-Ask-Human", "30s")
	r = agentd.AsAgentPeer(r, conv)
	resp := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, resp.Code,
		"a large approved copy must not fail post-approval; body=%s", resp.Body.String())
	require.Len(t, rec.texts, 1)
	assert.Equal(t, len(payload), len(rec.texts[0]), "the FULL payload must reach the writer, not a 64 KiB prefix")
	assert.Equal(t, payload, rec.texts[0])
}

// Scenario: a body past the wire cap is rejected before the decoded-length
// check — the pre-decode MaxBytesReader guard (mirrors notify-human's
// oversized-request test). Nothing is copied.
func TestClipboard_OversizedWireRequestRejected(t *testing.T) {
	f := newFlow(t)
	var rec clipRecorder
	rec.install(t)

	const conv = "clp9-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "worker")
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanClipboard, "test"))

	// Far past the ~1.5 MiB wire cap — MaxBytesReader trips mid-decode.
	huge := strings.Repeat("y", 2*1024*1024)
	r := testharness.JSONRequest(t, http.MethodPost, "/v1/clipboard",
		map[string]any{"text": huge})
	r = agentd.AsAgentPeer(r, conv)
	resp := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusBadRequest, resp.Code, "body=%s", resp.Body.String())
	assert.Empty(t, rec.texts)
}
