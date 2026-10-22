package agentd

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSubprocessErrorShapes(t *testing.T) {
	exitErr := errors.New("exit status 1")

	// GitHub's 502 arrives as an HTML page on stdout and an HTTP line on
	// stderr: both survive, whitespace-collapsed.
	page := []byte("<!DOCTYPE html>\n<html><head><title>Unicorn! &middot; GitHub</title></head><body>" +
		"No server is currently available to service your request.</body></html>")
	err := subprocessError(context.Background(), time.Second, exitErr, "gh: HTTP 502\n", page)
	assert.EqualError(t, err, "gh: HTTP 502; response: Unicorn! · GitHub: exit status 1")
	assert.ErrorIs(t, err, exitErr)
	assert.Equal(t, ghErrGitHub5xx, classifyGHError(err))

	// A JSON body is the caller's to decode; only stderr is kept.
	err = subprocessError(context.Background(), time.Second, exitErr,
		"gh: You have exceeded a secondary rate limit. Please wait a few minutes before you try again.",
		[]byte(`{"message":"You have exceeded a secondary rate limit"}`))
	assert.NotContains(t, err.Error(), "response:")
	assert.Equal(t, ghErrSecondaryRateLimit, classifyGHError(err))

	// Nothing to add: the bare error.
	assert.Same(t, exitErr, subprocessError(context.Background(), time.Second, exitErr, "  ", nil))

	// Our own deadline reads as a timeout, not "signal: killed".
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err = subprocessError(ctx, 12*time.Second, errors.New("signal: killed"), "", nil)
	assert.EqualError(t, err, "timed out after 12s")
	assert.Equal(t, ghErrTimeoutLocal, classifyGHError(err))

	// A plain-text body is truncated and stripped of control characters.
	long := []byte("oops\x1b[31m " + strings.Repeat("x", 300))
	snippet := nonJSONBodySnippet(long)
	assert.NotContains(t, snippet, "\x1b")
	assert.LessOrEqual(t, len(snippet), subprocessBodyMax+3)
}

func TestClassifyGHError(t *testing.T) {
	for msg, want := range map[string]string{
		"gh: API rate limit exceeded for user ID 1. (HTTP 403)":                                                                  ghErrRateLimit,
		"gh: We couldn't respond to your request in time. Sorry about that. (HTTP 504)":                                          ghErrGitHubTimeout,
		"gh: Something went wrong while executing your query. This may be the result of a timeout, or it could be a GitHub bug.": ghErrGitHubTimeout,
		"gh: HTTP 503: exit status 1":    ghErrGitHub5xx,
		"gh: Bad credentials (HTTP 401)": ghErrAuth,
		`Post "https://api.github.com/graphql": dial tcp: lookup api.github.com: no such host`: ghErrNetwork,
		"error connecting to api.github.com":                                                   ghErrNetwork,
		"decode tracked PR search: invalid character '<'":                                      ghErrDecode,
		"something else entirely":                                                              ghErrOther,
	} {
		assert.Equal(t, want, classifyGHError(errors.New(msg)), msg)
	}
	assert.Equal(t, ghErrGHMissing, classifyGHError(&exec.Error{Name: "gh", Err: exec.ErrNotFound}))
	assert.Empty(t, classifyGHError(nil))
}

func TestRunInDirWithTimeoutReportsTimeout(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep binary")
	}
	_, err := runInDirWithTimeout("", 50*time.Millisecond, "sleep", "5")
	assert.EqualError(t, err, "timed out after 50ms")
}
