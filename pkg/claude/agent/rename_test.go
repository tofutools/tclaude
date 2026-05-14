package agent

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunRename_AutoAndTitleAreMutuallyExclusive: passing both --auto
// and a positional title should bail out before any daemon I/O.
func TestRunRename_AutoAndTitleAreMutuallyExclusive(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runRename(&renameParams{
		Title: "explicit-title",
		Auto:  true,
	}, &stdout, &stderr)
	require.Equal(t, rcInvalidArg, rc, "stdout=%q stderr=%q", stdout.String(), stderr.String())
	assert.Contains(t, stderr.String(), "mutually exclusive", "stderr should explain the conflict")
}

// TestRunRename_EmptyTitleWithoutAutoRejected: the existing
// title-validation path still rejects empty / malformed titles when
// --auto is not set.
func TestRunRename_EmptyTitleWithoutAutoRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runRename(&renameParams{
		Title: "",
		Auto:  false,
	}, &stdout, &stderr)
	assert.Equal(t, rcInvalidArg, rc, "empty title without --auto should be rejected")
}

// TestRunRename_AutoSkipsLocalTitleValidation: with --auto set, an
// empty title should NOT trigger the local title-charset rejection
// and the daemon request should carry `auto: true`, not `title`.
// Stubs DaemonRequestImpl so this doesn't depend on a real daemon
// being up.
func TestRunRename_AutoSkipsLocalTitleValidation(t *testing.T) {
	prevAvail := DaemonAvailableImpl
	prevReq := DaemonRequestImpl
	defer func() {
		DaemonAvailableImpl = prevAvail
		DaemonRequestImpl = prevReq
	}()

	DaemonAvailableImpl = func() bool { return true }
	var capturedBody map[string]any
	DaemonRequestImpl = func(method, path string, in, out any, _ DaemonOpts) error {
		if m, ok := in.(map[string]any); ok {
			capturedBody = m
		}
		// Surface a minimal success response so the helper finishes.
		if resp, ok := out.(*struct {
			ConvID     string `json:"conv_id"`
			CallerConv string `json:"caller_conv,omitempty"`
			Title      string `json:"title"`
			Auto       bool   `json:"auto,omitempty"`
			Note       string `json:"note,omitempty"`
		}); ok {
			resp.ConvID = "fake-conv"
			resp.Auto = true
		}
		return nil
	}

	var stdout, stderr bytes.Buffer
	rc := runRename(&renameParams{
		Title: "",
		Auto:  true,
	}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	assert.NotContains(t, stderr.String(), "REJECTED. Title must be", "auto path should NOT hit the title-charset rejection")
	assert.Equal(t, true, capturedBody["auto"], "daemon body should include auto:true, got %#v", capturedBody)
	_, hasTitle := capturedBody["title"]
	assert.False(t, hasTitle, "daemon body should NOT include title on auto path, got %#v", capturedBody)
}
