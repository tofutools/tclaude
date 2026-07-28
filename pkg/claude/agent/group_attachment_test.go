package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunGroupAttachmentSetForwardsURLLabelAndApproval(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }

	var gotBody map[string]any
	var gotOpts DaemonOpts
	DaemonRequestImpl = func(method, path string, in, out any, opts DaemonOpts) error {
		assert.Equal(t, http.MethodPost, method)
		assert.Equal(t, "/v1/groups/platform%20team/attachment", path)
		gotBody, _ = in.(map[string]any)
		gotOpts = opts
		return json.Unmarshal([]byte(`{
			"group":"platform team",
			"attachment_url":"https://linear.app/acme/project/platform",
			"attachment_label":"Platform"
		}`), out)
	}

	var stdout, stderr bytes.Buffer
	rc := runGroupAttachmentSet(&groupAttachmentSetParams{
		Group:    "platform team",
		URL:      " https://linear.app/acme/project/platform ",
		Label:    " Platform ",
		AskHuman: "30s",
	}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.Equal(t, "https://linear.app/acme/project/platform", gotBody["url"])
	assert.Equal(t, "Platform", gotBody["label"])
	assert.Equal(t, 30*time.Second, gotOpts.AskHuman)
	assert.Contains(t, stdout.String(), "attachment set")
}

func TestRunGroupAttachmentClearForwardsClear(t *testing.T) {
	prevAvail, prevReq := DaemonAvailableImpl, DaemonRequestImpl
	t.Cleanup(func() { DaemonAvailableImpl, DaemonRequestImpl = prevAvail, prevReq })
	DaemonAvailableImpl = func() bool { return true }

	DaemonRequestImpl = func(method, path string, in, out any, _ DaemonOpts) error {
		assert.Equal(t, http.MethodPost, method)
		assert.Equal(t, "/v1/groups/team/attachment", path)
		assert.Equal(t, map[string]any{"clear": true}, in)
		return json.Unmarshal([]byte(`{"group":"team","cleared":true}`), out)
	}

	var stdout, stderr bytes.Buffer
	rc := runGroupAttachmentClear(
		&groupAttachmentClearParams{Group: "team"}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.Contains(t, stdout.String(), "attachment cleared")
}
