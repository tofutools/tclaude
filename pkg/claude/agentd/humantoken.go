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
// daemon restart mints a fresh one and the human re-fetches it.
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

// handleAuthToken serves the operator token to the human operator so the
// CLI (`tclaude agent token`) can bootstrap authentication.
//
// !!! DELIBERATE EXCEPTION — DO NOT route this through classify() / !!!
// !!! requireHuman.                                                  !!!
// classify() recognises the human ONLY by a valid operator token, and
// this endpoint is how the human OBTAINS that token. Routing it through
// classify() would be circular — you would need the token to fetch the
// token. It is therefore gated on the legacy heuristic: a caller with NO
// claude/node ancestor in its process tree. That is sound for the threat
// model — a sandboxed agent always has a Claude Code ancestor in agentd's
// host-side /proc walk (the bwrap PID namespace cannot be escaped), so it
// is refused here and cannot bootstrap a token. A non-sandboxed detached
// process could fetch it, but that is the accepted same-uid residual
// (see docs/plans/agentd.md, "Security model"). A future reader must not
// "helpfully" route this through classify().
func handleAuthToken(w http.ResponseWriter, r *http.Request) {
	p := peerFromContext(r.Context())
	if p.PID == 0 {
		writeError(w, http.StatusUnauthorized, "auth",
			"could not determine peer PID; refusing to release the operator token")
		return
	}
	// Legacy heuristic, ON PURPOSE — see the function doc above.
	if p.HasClaudeAncestor {
		writeError(w, http.StatusForbidden, "auth",
			"the operator token authenticates the human operator; an agent caller cannot fetch it")
		return
	}
	tok := currentOperatorToken()
	if tok == "" {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"operator token not initialised")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
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
// startup banner — but ONLY when stdout is a real terminal. When stdout
// is not a TTY (the daemon was backgrounded or its output redirected,
// e.g. into ~/.tclaude/output.log) it prints just a pointer, never the
// token, so the secret can never land in a log file. Either way the
// token is retrievable with `tclaude agent token`.
func printOperatorTokenBanner(tok string) {
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Printf("  operator token:         %s\n", tok)
		fmt.Printf("    the human sets it with: export %s=\"$(tclaude agent token)\"\n", humanTokenEnvVar)
	} else {
		fmt.Printf("  operator token ready — fetch with `tclaude agent token` (not printed: stdout is not a terminal)\n")
	}
}
