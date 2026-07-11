// Package agentipctest holds test-only helpers for exercising the agentd Unix
// socket surface. It is imported from *_test.go files across several packages,
// so it lives in a normal (non-_test) file.
package agentipctest

import (
	"os"
	"testing"
)

// socketEnv is duplicated here rather than importing the parent agentipc
// package: agentipc's own tests use this helper, so importing the parent would
// create a test-time package cycle.
const socketEnv = "TCLAUDE_AGENTD_SOCKET"

// IsolateSocketEnv removes an inherited daemon-socket override for the life of
// a test. Managed agent sessions export the real daemon address; tests that
// replace HOME or create test sockets must not accidentally dial or validate
// against that live endpoint.
func IsolateSocketEnv(t *testing.T) {
	t.Helper()
	t.Setenv(socketEnv, "")
}

// maxSocketPathLen is a conservative bound for the length of a Unix socket path
// (the sun_path field). Linux allows 108 bytes and darwin 104; we use the
// smaller so a path that fits here fits on both.
const maxSocketPathLen = 104

// socketSuffixHeadroom reserves room, below the directory this helper returns,
// for the longest socket path any caller derives from it. The deepest is a
// HOME-relative canonical socket "<dir>/.tclaude/api/agentd.sock" (25 bytes);
// we round up for margin (and the sun_path NUL terminator).
const socketSuffixHeadroom = 32

// candidateBases lists the base directories ShortSocketDir probes, in priority
// order. The first that is writable AND yields a short-enough path wins.
//
// The order matters under a restricted sandbox (tclaude's own sandbox-profile
// feature included) where arbitrary /tmp is denied but $TMPDIR and /tmp/claude
// are writable — hence they come first. os.TempDir() is the last resort; on
// macOS it is a long /var/folders/… path that usually fails the length check,
// which is exactly why /tmp is preferred for short socket paths.
func candidateBases() []string {
	return []string{
		os.Getenv("TMPDIR"),
		"/tmp/claude",
		"/tmp",
		os.TempDir(),
	}
}

// ShortSocketDir first clears any inherited live-daemon socket override, then
// returns a fresh temp directory whose path is short enough that
// a Unix socket created a few levels below it stays within the sun_path limit,
// and which is writable under a restricted filesystem sandbox.
//
// It exists because neither hardcoding /tmp nor t.TempDir() works everywhere:
// /tmp is denied under a locked-down sandbox, while macOS t.TempDir() returns
// long /var/folders/…/T/TestName/NNN/ paths that overflow sun_path. This probes
// candidateBases() and uses the first that is both writable and short enough,
// registering cleanup for the winner. If none qualify it t.Skips with a clear
// message rather than erroring — a socket test cannot run without a short
// writable dir, but that is an environment limitation, not a failure.
func ShortSocketDir(t *testing.T) string {
	t.Helper()
	IsolateSocketEnv(t)
	for _, base := range candidateBases() {
		if dir, ok := tryBase(t, base); ok {
			return dir
		}
	}
	t.Skipf("no writable base dir yields a socket path within the %d-byte sun_path limit; tried %v",
		maxSocketPathLen, candidateBases())
	return ""
}

// tryBase attempts to create a temp dir under base, returning it (with cleanup
// registered) only if base is writable and the resulting path leaves room for a
// socket below it. Any failure returns ok=false so the caller moves on.
func tryBase(t *testing.T, base string) (string, bool) {
	if base == "" {
		return "", false
	}
	// The base may not exist yet (e.g. /tmp/claude on a fresh sandbox); create
	// it so MkdirTemp can use it. Ignore the error — a genuinely unwritable base
	// fails the MkdirTemp below and we move on to the next candidate.
	_ = os.MkdirAll(base, 0o700)
	dir, err := os.MkdirTemp(base, "tc-sock-")
	if err != nil {
		return "", false
	}
	if len(dir)+socketSuffixHeadroom > maxSocketPathLen {
		_ = os.RemoveAll(dir)
		return "", false
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir, true
}
