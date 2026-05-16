package agentd_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/humannotify"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// fakeHumanTransport is a humannotify.Transport that records sends in
// memory instead of calling a real external API — the injected
// boundary for the notify-human flow tests. Send is driven synchronously
// by the httptest handler, so no locking is needed.
type fakeHumanTransport struct {
	name    string
	sent    []humannotify.Notification
	sendErr error
}

func (f *fakeHumanTransport) Name() string { return f.name }

func (f *fakeHumanTransport) Send(_ context.Context, n humannotify.Notification) (string, error) {
	if f.sendErr != nil {
		return "", f.sendErr
	}
	f.sent = append(f.sent, n)
	return fmt.Sprintf("handle-%d", len(f.sent)), nil
}

// installTransport swaps the daemon's human-notify transport resolver
// so handleNotifyHuman runs against fake / err instead of the real
// humannotify.Resolve. Restored via t.Cleanup.
func installTransport(t *testing.T, fake humannotify.Transport, err error) {
	t.Helper()
	t.Cleanup(agentd.SetHumanNotifyTransportForTest(
		func(*config.Config) (humannotify.Transport, error) { return fake, err },
	))
}

// Scenario: a PO holding human.notify sends a notification. The daemon
// gates on the slug, resolves the configured transport, and delivers.
// Real surface: the transport receives a Notification carrying the
// caller's conv-id, title, and group for the human-facing attribution.
func TestNotifyHuman_GrantedSenderDelivers(t *testing.T) {
	f := newFlow(t)

	const poConv = "po00-1111-2222-3333-4444"
	f.HaveConvWithTitle(poConv, "tclaude-PO")
	f.HaveGroup("tclaude-dev")
	f.HaveMember("tclaude-dev", poConv)
	require.NoError(t, db.GrantAgentPermission(poConv, agentd.PermHumanNotify, "test"))

	fake := &fakeHumanTransport{name: "telegram-fake"}
	installTransport(t, fake, nil)

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "CI is green; PR #142 up for review", "subject": "status"})
	r = agentd.AsAgentPeer(r, poConv)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		Transport string `json:"transport"`
		Delivered bool   `json:"delivered"`
		Handle    string `json:"handle"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	assert.Equal(t, "telegram-fake", resp.Transport)
	assert.True(t, resp.Delivered)
	assert.NotEmpty(t, resp.Handle, "the transport's message handle should be echoed back")

	require.Len(t, fake.sent, 1, "exactly one notification should have been sent")
	n := fake.sent[0]
	assert.Equal(t, "CI is green; PR #142 up for review", n.Body)
	assert.Equal(t, "status", n.Subject)
	assert.Equal(t, poConv, n.FromConv, "caller conv-id must reach the transport")
	assert.Equal(t, "tclaude-PO", n.FromTitle, "caller title must reach the transport")
	assert.Equal(t, "tclaude-dev", n.Group, "caller group must reach the transport")
}

// Scenario: a worker that does NOT hold human.notify is refused — the
// permission slug is the anti-spam control. Nothing reaches the
// transport.
func TestNotifyHuman_UngrantedSenderForbidden(t *testing.T) {
	f := newFlow(t)

	const workerConv = "wk00-1111-2222-3333-4444"
	f.HaveGroup("tclaude-dev")
	f.HaveMember("tclaude-dev", workerConv)

	fake := &fakeHumanTransport{name: "telegram-fake"}
	installTransport(t, fake, nil)

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "let me spam the human"})
	r = agentd.AsAgentPeer(r, workerConv)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())

	var errResp struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	testharness.DecodeJSON(t, rec, &errResp)
	assert.Contains(t, errResp.Error, agentd.PermHumanNotify,
		"the 403 should name the missing slug")
	assert.Empty(t, fake.sent, "a denied caller must not reach the transport")
}

// Scenario: the human (no Claude ancestor) is implicitly allowed — they
// bypass the slug gate, same convention as every other gated endpoint.
func TestNotifyHuman_HumanBypassesSlug(t *testing.T) {
	f := newFlow(t)

	fake := &fakeHumanTransport{name: "telegram-fake"}
	installTransport(t, fake, nil)

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "human-initiated ping"})
	r = agentd.AsHumanPeer(r)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Len(t, fake.sent, 1)
	assert.Empty(t, fake.sent[0].FromConv, "the human path has no caller conv-id")
}

// Scenario: no transport is configured. The endpoint reports it as a
// precondition failure with the not_configured code, so the CLI can
// point the human at config.json — not a generic 500.
func TestNotifyHuman_NotConfigured(t *testing.T) {
	f := newFlow(t)

	installTransport(t, nil, humannotify.ErrNotConfigured)

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "anyone there?"})
	r = agentd.AsHumanPeer(r)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, "body=%s", rec.Body.String())

	var errResp struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	testharness.DecodeJSON(t, rec, &errResp)
	assert.Equal(t, "not_configured", errResp.Code)
}

// Scenario: the transport itself fails (e.g. Telegram API rejected the
// send). The endpoint surfaces it as a 502 with the upstream error,
// rather than swallowing the failure as a success.
func TestNotifyHuman_TransportFailure(t *testing.T) {
	f := newFlow(t)

	fake := &fakeHumanTransport{name: "telegram-fake", sendErr: fmt.Errorf("telegram sendMessage rejected: chat not found")}
	installTransport(t, fake, nil)

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "this will fail to send"})
	r = agentd.AsHumanPeer(r)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusBadGateway, rec.Code, "body=%s", rec.Body.String())

	var errResp struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	testharness.DecodeJSON(t, rec, &errResp)
	assert.Equal(t, "transport_failed", errResp.Code)
	assert.Contains(t, errResp.Error, "chat not found")
}

// Scenario: an empty body is a client error — caught before the
// transport is touched.
func TestNotifyHuman_EmptyBodyRejected(t *testing.T) {
	f := newFlow(t)

	fake := &fakeHumanTransport{name: "telegram-fake"}
	installTransport(t, fake, nil)

	r := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "   "})
	r = agentd.AsHumanPeer(r)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Empty(t, fake.sent, "an empty body must not reach the transport")
}

// Scenario: a non-POST method is refused.
func TestNotifyHuman_MethodNotAllowed(t *testing.T) {
	f := newFlow(t)
	installTransport(t, &fakeHumanTransport{name: "telegram-fake"}, nil)

	r := testharness.JSONRequest(t, http.MethodGet, "/v1/notify-human", nil)
	r = agentd.AsHumanPeer(r)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code, "body=%s", rec.Body.String())
}
