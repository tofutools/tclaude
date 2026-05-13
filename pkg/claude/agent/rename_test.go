package agent

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunRename_AutoAndTitleAreMutuallyExclusive: passing both --auto
// and a positional title should bail out before any daemon I/O.
func TestRunRename_AutoAndTitleAreMutuallyExclusive(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := runRename(&renameParams{
		Title: "explicit-title",
		Auto:  true,
	}, &stdout, &stderr)
	if rc != rcInvalidArg {
		t.Fatalf("rc = %d, want rcInvalidArg (%d). stdout=%q stderr=%q",
			rc, rcInvalidArg, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Errorf("stderr should explain the conflict, got %q", stderr.String())
	}
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
	if rc != rcInvalidArg {
		t.Errorf("empty title without --auto should be rejected, got rc=%d", rc)
	}
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
	if rc != rcOK {
		t.Fatalf("rc = %d, want rcOK. stderr=%q", rc, stderr.String())
	}
	if strings.Contains(stderr.String(), "REJECTED. Title must be") {
		t.Errorf("auto path should NOT hit the title-charset rejection, got: %q",
			stderr.String())
	}
	if capturedBody["auto"] != true {
		t.Errorf("daemon body should include auto:true, got %#v", capturedBody)
	}
	if _, hasTitle := capturedBody["title"]; hasTitle {
		t.Errorf("daemon body should NOT include title on auto path, got %#v", capturedBody)
	}
}
