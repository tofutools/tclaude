package agentd_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These pin what each agent-initiated approval popup shows the human and what
// a denial answers, so the shared request builder cannot shift a field, a
// refusal text, the scope or the "always allow" offer at any gate site.

const (
	popupCaller = "popup-caller-aaaa-bbbb-cccc-111111111111"
	popupTarget = "popup-target-bbbb-cccc-dddd-222222222222"
)

func popupFlow(t *testing.T, decision bool) (*testharness.Flow, func() []agentd.ApprovalPopupFields) {
	t.Helper()
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveConvWithTitle(popupCaller, "popup-caller")
	f.HaveConvWithTitle(popupTarget, "popup-target")
	f.HaveAliveSession(popupTarget, "spwn-popup-target", "tmux-popup-target", t.TempDir())
	f.MarkOffline("tmux-popup-target")
	got, restore := agentd.CaptureApprovalPopupForTest(decision)
	t.Cleanup(restore)
	return f, got
}

func askHuman(t *testing.T, f *testharness.Flow, method, path string, body any) (int, string) {
	t.Helper()
	r := agentd.AsAgentPeer(testharness.JSONRequest(t, method, path, body), popupCaller)
	r.Header.Set("X-Tclaude-Ask-Human", "5s")
	rec := testharness.Serve(f.Mux, r)
	return rec.Code, rec.Body.String()
}

func TestApprovalPopup_SelfPermission(t *testing.T) {
	f, got := popupFlow(t, false)
	code, body := askHuman(t, f, http.MethodPost, "/v1/clipboard?via=test", map[string]any{"text": "copy me"})
	assert.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, `human declined or timed out after 5s on permission \"human.clipboard\"`)
	require.Len(t, got(), 1)
	c := got()[0]
	assert.Equal(t, agentd.ApprovalPopupFields{
		Perm: agentd.PermHumanClipboard, ConvID: popupCaller, ConvTitle: "popup-caller",
		Method: http.MethodPost, Path: "/v1/clipboard", RawQuery: "via=test",
		BodyPreview: "copy me", BodyLabel: "Clipboard content",
		AutoGrantable: agentd.IsAutoGrantableSlug(agentd.PermHumanClipboard),
		Timeout:       c.Timeout, HasChannels: true,
	}, c)
	assert.Positive(t, c.Timeout)
}

func TestApprovalPopup_CrossAgentPermission(t *testing.T) {
	f, got := popupFlow(t, false)
	code, body := askHuman(t, f, http.MethodPost, "/v1/agent/"+url.PathEscape(popupTarget)+"/resume", nil)
	assert.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, `human declined or timed out after 5s on cross-agent permission \"agent.resume\" for target popup-ta`)
	require.Len(t, got(), 1)
	c := got()[0]
	assert.Equal(t, agentd.ApprovalPopupFields{
		Perm: agentd.PermAgentResume, ConvID: popupCaller, ConvTitle: "popup-caller",
		Method: http.MethodPost, Path: "/v1/agent/" + popupTarget + "/resume",
		BodyPreview: c.BodyPreview, BodyLabel: c.BodyLabel,
		TargetConvID: popupTarget, TargetConvTitle: "popup-target",
		AutoGrantable: false, // "always allow" would grant the slug against every target
		Timeout:       c.Timeout, HasChannels: true,
	}, c)
}

func TestApprovalPopup_ResumeTargetActions(t *testing.T) {
	for _, tc := range []struct {
		name, query, preview, label, declined, noPopup string
	}{
		{
			name: "recreate launch dir", query: "recreate=1",
			preview:  "Recreate the stopped target's missing launch directory for this resume.",
			label:    "Resume launch-directory recreation",
			declined: "human declined or timed out after 5s while recreating the launch directory for target popup-ta",
			noPopup:  "no popup base URL configured; launch-directory recreation cannot be approved",
		},
		{
			name: "codex drive rollback", query: "send_keys=1",
			preview:  "Durably change the stopped target from the Codex app-server drive to tmux send-keys, tear down its recorded app-server runtime, and resume it. Future relaunches will remain on send-keys until explicitly changed again.",
			label:    "Codex drive rollback (app-server → send-keys)",
			declined: "human declined or timed out after 5s while approving Codex drive rollback for target popup-ta",
			noPopup:  "no popup base URL configured; Codex drive rollback cannot be approved",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, got := popupFlow(t, false)
			require.NoError(t, db.GrantAgentPermission(popupCaller, agentd.PermAgentResume, "test"))
			path := "/v1/agent/" + url.PathEscape(popupTarget) + "/resume?" + tc.query
			code, body := askHuman(t, f, http.MethodPost, path, nil)
			assert.Equal(t, http.StatusForbidden, code)
			assert.Contains(t, body, tc.declined)
			require.Len(t, got(), 1, "the granted cross-agent gate opens no popup; only the action's own does")
			c := got()[0]
			assert.Equal(t, agentd.ApprovalPopupFields{
				Perm: agentd.PermAgentResume, ConvID: popupCaller, ConvTitle: "popup-caller",
				Method: http.MethodPost, Path: "/v1/agent/" + popupTarget + "/resume", RawQuery: tc.query,
				BodyPreview: tc.preview, BodyLabel: tc.label,
				TargetConvID: popupTarget, TargetConvTitle: "popup-target",
				Timeout: c.Timeout, HasChannels: true,
			}, c)

			t.Cleanup(agentd.SetPopupBaseURLForTest(""))
			code, body = askHuman(t, f, http.MethodPost, path, nil)
			assert.Equal(t, http.StatusForbidden, code)
			assert.Contains(t, body, tc.noPopup)
			assert.Len(t, got(), 1, "no popup without a popup URL")
		})
	}
}
