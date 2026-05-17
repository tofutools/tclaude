package agentd

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
)

// The operator token positively authenticates the human operator on the
// CLI / Unix-socket path, so the daemon can fail closed instead of
// assuming "no Claude Code ancestor => human". See docs/plans/agentd.md,
// "Security model".
//
// THREAT MODEL — do not over-claim. The token is a real boundary only in
// composition with the OS sandbox. A sandboxed (bwrap PID-namespace) agent
// cannot read the human's environment and so cannot obtain the token; for
// it, fail-closed + token is a genuine boundary. A NON-sandboxed same-uid
// process can read /proc/<human-pid>/environ and therefore the token — and
// can mutate ~/.tclaude state directly anyway — so against it the token is
// not a boundary. The token gates the human path; the OS sandbox confines
// the agent; neither is a standalone boundary.

const (
	// humanTokenHeader carries the operator token on `tclaude agent`
	// requests. Custom-header style matches X-Tclaude-Ask-Human et al.
	humanTokenHeader = "X-Tclaude-Human-Token"
	// humanTokenEnvVar is the environment variable the CLI reads the
	// operator token from.
	humanTokenEnvVar = "TCLAUDE_HUMAN_TOKEN"
	// humanTokenPrefix marks an operator token. Aids secret-scanners and
	// lets the verifier fast-reject obviously-malformed input.
	humanTokenPrefix = "tclo_"
)

// operatorToken is the per-daemon-lifetime operator token. Generated once
// at startup by generateOperatorToken, held only in memory — never
// persisted to disk, never written through slog (slog → output.log). A
// daemon restart mints a fresh one; the human re-reads it from the
// startup banner.
var (
	operatorTokenMu sync.RWMutex
	operatorToken   string
)

// generateOperatorToken mints a fresh operator token (32 bytes of
// crypto/rand, base64url, humanTokenPrefix) and stores it. Called once at
// daemon startup. Panics if crypto/rand fails — an auth token the daemon
// cannot generate is unrecoverable.
func generateOperatorToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic("agentd: crypto/rand failed generating operator token: " + err.Error())
	}
	tok := humanTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	operatorTokenMu.Lock()
	operatorToken = tok
	operatorTokenMu.Unlock()
	return tok
}

// currentOperatorToken returns the operator token, or "" if one has not
// been generated yet (e.g. in tests that never call generateOperatorToken).
func currentOperatorToken() string {
	operatorTokenMu.RLock()
	defer operatorTokenMu.RUnlock()
	return operatorToken
}

// verifyHumanToken reports whether r carries a valid operator token in
// humanTokenHeader. Constant-time compare. False when the header is
// absent, when no token has been generated, or on any mismatch.
func verifyHumanToken(r *http.Request) bool {
	got := strings.TrimSpace(r.Header.Get(humanTokenHeader))
	if got == "" {
		return false
	}
	want := currentOperatorToken()
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// spawnEnvWithoutOperatorToken returns the current process environment
// with TCLAUDE_HUMAN_TOKEN stripped. agentd uses it for the environment
// of every CC session it spawns: the operator token authenticates the
// human, and an agent must never inherit it. classify() already makes
// agent-ness win over the token, so this is sec.10 defence-in-depth — it
// keeps the secret out of agent environments entirely rather than
// relying solely on the classification precedence.
func spawnEnvWithoutOperatorToken() []string {
	all := os.Environ()
	out := make([]string, 0, len(all))
	for _, kv := range all {
		if strings.HasPrefix(kv, humanTokenEnvVar+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// printOperatorTokenBanner writes the operator token to the daemon's
// startup banner. The banner is the SOLE delivery channel — there is no
// fetch endpoint — so it prints a ready-to-paste `export` line when
// stdout is a real terminal.
//
// When stdout is NOT a TTY (the daemon was backgrounded or its output
// redirected, e.g. into ~/.tclaude/output.log) it must never print the
// token — it could land in a log file — and the token is not retrievable
// any other way, so it tells the operator to relaunch agentd attached to
// a terminal.
func printOperatorTokenBanner(tok string) {
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Printf("  operator token — the human sets it with:\n")
		fmt.Printf("    export %s=%q\n", humanTokenEnvVar, tok)
	} else {
		fmt.Printf("  operator token issued — relaunch agentd attached to a terminal to see it\n")
	}
}
