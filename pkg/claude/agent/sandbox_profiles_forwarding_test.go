package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The daemon is the only place that refuses retired input, so the CLI has to
// hand it the operator's bytes unchanged. These tests exist because the first
// cut of TCL-791 did not: profile files were decoded into the client-side
// sandboxProfileJSON, which had already lost the break-glass fields, so
// `create -f` stripped break_glass_filesystem and exited 0 with "Created
// sandbox profile". The removal was silent at exactly the surface an operator
// hand-edits.
//
// The property under test is deliberately broader than break-glass: the CLI
// must forward the document verbatim, so any field the daemon wants to refuse
// reaches it. A future tombstone should need no change here.

// profileWithRetiredField is the payload an operator upgrading from a
// break-glass-era profile still has on disk.
const profileWithRetiredField = `{
	"name": "debug",
	"filesystem": [],
	"environment": [],
	"break_glass_filesystem": [{"path": "/home/op/.tclaude/data", "access": "write"}]
}`

func TestSandboxProfileCreateForwardsRetiredFieldsToTheDaemon(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(string, string) (int, string, string) {
		return http.StatusUnprocessableEntity, "break_glass_removed", ""
	})

	var stdout, stderr bytes.Buffer
	rc := runSandboxProfilesCreate(&sandboxProfilesFileParams{File: "-"},
		strings.NewReader(profileWithRetiredField), &stdout, &stderr)

	require.NotEqual(t, rcOK, rc, "a refused create must not report success")
	assert.NotContains(t, stdout.String(), "Created sandbox profile")
	require.Len(t, calls, 1)
	assertForwardedVerbatim(t, calls[0].body)
}

func TestSandboxProfileEditForwardsRetiredFieldsToTheDaemon(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(string, string) (int, string, string) {
		return http.StatusUnprocessableEntity, "break_glass_removed", ""
	})

	var stdout, stderr bytes.Buffer
	rc := runSandboxProfilesEdit(&sandboxProfilesEditParams{Name: "debug", File: "-"},
		strings.NewReader(profileWithRetiredField), &stdout, &stderr)

	require.NotEqual(t, rcOK, rc, "a refused edit must not report success")
	assert.NotContains(t, stdout.String(), "Updated sandbox profile")
	require.Len(t, calls, 1)
	assertForwardedVerbatim(t, calls[0].body)
}

func TestSandboxProfileDraftForwardsRetiredFieldsToTheDaemon(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(string, string) (int, string, string) {
		return http.StatusUnprocessableEntity, "break_glass_removed", ""
	})

	var stdout, stderr bytes.Buffer
	rc := runSandboxProfilesDraft(&sandboxProfilesDraftParams{
		Token: "abcdefghijklmnop", File: "-",
	}, strings.NewReader(profileWithRetiredField), &stdout, &stderr)

	require.NotEqual(t, rcOK, rc, "a refused draft must not report success")
	assert.NotContains(t, stdout.String(), "has not been saved")
	require.Len(t, calls, 1)
	wrapper, ok := calls[0].body.(struct {
		Profile json.RawMessage `json:"profile"`
	})
	require.True(t, ok, "draft must wrap the untouched document, got %T", calls[0].body)
	assertForwardedVerbatim(t, wrapper.Profile)
}

// assertForwardedVerbatim re-marshals what the CLI handed DaemonRequest and
// checks the retired field survived. Marshalling is the point: a struct that
// merely holds the field would still drop it on the way out.
func assertForwardedVerbatim(t *testing.T, body any) {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	assert.JSONEq(t, profileWithRetiredField, string(encoded),
		"the CLI must forward the operator's document unchanged so the daemon can refuse it")
}

// A malformed file still has to fail locally with a useful message rather than
// being shipped to the daemon as a bad request.
func TestSandboxProfileCreateRejectsMalformedFileLocally(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"not json", `{`, "not valid sandbox-profile JSON"},
		{"not an object", `["nope"]`, "not valid sandbox-profile JSON"},
		{"json null", `null`, "must contain a sandbox-profile JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []capturedReq
			stubDaemon(t, &calls, ok(`{}`))

			var stdout, stderr bytes.Buffer
			rc := runSandboxProfilesCreate(&sandboxProfilesFileParams{File: "-"},
				strings.NewReader(tc.input), &stdout, &stderr)

			assert.Equal(t, rcInvalidArg, rc)
			assert.Contains(t, stderr.String(), tc.want)
			assert.Empty(t, calls, "a malformed file must never reach the daemon")
		})
	}
}
