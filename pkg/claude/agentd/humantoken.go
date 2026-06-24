package agentd

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/zalando/go-keyring"
)

// The operator token positively authenticates the human operator on the
// CLI / Unix-socket path, so the daemon can fail closed instead of
// assuming "no Claude Code ancestor => human".
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

// operatorToken is the live operator token for this daemon lifetime. It is
// installed once at startup and held in memory; it is never written through
// slog (slog → output.log). By default it is also never persisted to disk
// and a restart mints a fresh one; with persistence opted in (see the
// "Persistent operator token" section below) the SAME value is restored
// across restarts from the keychain / 0600 file. Either way the in-memory
// copy here is what verifyHumanToken compares against.
var (
	operatorTokenMu sync.RWMutex
	operatorToken   string
)

// mintOperatorToken produces a fresh operator token (32 bytes of
// crypto/rand, base64url, humanTokenPrefix) WITHOUT storing it. Panics if
// crypto/rand fails — an auth token the daemon cannot generate is
// unrecoverable.
func mintOperatorToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic("agentd: crypto/rand failed generating operator token: " + err.Error())
	}
	return humanTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
}

// setOperatorToken installs tok as the live operator token under the lock.
// Used by both the ephemeral (generateOperatorToken) and the persisted
// (resolveOperatorToken) paths so the verifier/banner are agnostic to how
// the token was sourced.
func setOperatorToken(tok string) {
	operatorTokenMu.Lock()
	operatorToken = tok
	operatorTokenMu.Unlock()
}

// generateOperatorToken mints a fresh operator token and stores it.
// This is the default (non-persisted) startup path — the token lives only
// in memory and a restart mints a new one. Returns the token.
func generateOperatorToken() string {
	tok := mintOperatorToken()
	setOperatorToken(tok)
	return tok
}

// ---- Persistent operator token (opt-in) -----------------------------------
//
// By default the operator token is ephemeral (generateOperatorToken): a
// fresh one each daemon lifetime, lost on exit, so the human re-exports
// TCLAUDE_HUMAN_TOKEN after every restart. The human can opt into a STABLE
// token (config agent.persist_operator_token / `--persist-operator-token`)
// that survives restarts. The daemon stores it in the OS keychain when one
// is reachable, otherwise a 0600 file under ~/.tclaude.
//
// STORAGE THREAT MODEL — the file fallback keeps the same boundary as the
// in-memory token: the agent sandbox denies reads to ~/.tclaude (only the
// agentd socket is carved out), so a sandboxed agent cannot read the file;
// a non-sandboxed same-uid process could, but it can already read the
// human's /proc/<pid>/environ and mutate ~/.tclaude state, so the token was
// never a boundary against it (see the package-level note above). The
// keychain path narrows agent access further where the platform blocks it.
// Encrypting the file with a key that also lives on the same machine would
// add no boundary (the key sits in the same trust zone) — real at-rest
// encryption needs the OS keychain, which is exactly the keychain branch.

const (
	// keychainService / keychainUser identify the operator-token secret in
	// the OS keychain (macOS Keychain, Linux Secret Service, Windows
	// Credential Manager) via go-keyring.
	keychainService = "tclaude"
	keychainUser    = "operator_token"
)

// keychainGet / keychainSet are indirected through vars so tests can swap
// in a fake backend (the real keychain needs a desktop session / D-Bus that
// CI and unit tests don't have). Production wires the go-keyring funcs.
var (
	keychainGet = keyring.Get
	keychainSet = keyring.Set
)

// tokenSourceKind names where the live operator token came from. It is
// logged and drives the startup banner wording.
type tokenSourceKind string

const (
	tokenSourceEphemeral tokenSourceKind = "ephemeral"
	tokenSourceKeychain  tokenSourceKind = "keychain"
	tokenSourceFile      tokenSourceKind = "file"
)

// tokenSource describes how the operator token was sourced this startup.
type tokenSource struct {
	kind tokenSourceKind
	path string // populated only when kind == tokenSourceFile
}

// shouldPersistOperatorToken reports whether the human opted into a stable,
// restart-surviving operator token. The `--persist-operator-token` flag and
// the agent.persist_operator_token config field OR together — either turns
// it on — mirroring the auto-launch-dashboard knob.
func shouldPersistOperatorToken(flagSet bool, cfg *config.Config) bool {
	if flagSet {
		return true
	}
	return cfg != nil && cfg.Agent != nil && cfg.Agent.PersistOperatorToken
}

// resolveOperatorToken sources the operator token for this daemon lifetime
// and installs it as the live token. persist=false keeps the historical
// behaviour (a fresh in-memory token); persist=true loads or creates a
// stable one (keychain, else file). Returns the token and where it came
// from.
func resolveOperatorToken(persist bool) (string, tokenSource) {
	if !persist {
		return generateOperatorToken(), tokenSource{kind: tokenSourceEphemeral}
	}
	tok, src := loadOrCreateOperatorToken()
	setOperatorToken(tok)
	return tok, src
}

// loadOrCreateOperatorToken returns a stable operator token, preferring the
// OS keychain and falling back to a 0600 ~/.tclaude/operator_token file when
// no keychain backend is reachable (headless Linux / WSL without D-Bus, …).
// If persistence fails outright it degrades to an ephemeral token rather
// than leaving the daemon without one.
func loadOrCreateOperatorToken() (string, tokenSource) {
	switch tok, found, err := keychainLookup(); {
	case err != nil:
		// Backend unreachable — fall through to the file fallback.
		slog.Info("operator token: OS keychain unavailable, using file fallback", "err", err)
	case found:
		return tok, tokenSource{kind: tokenSourceKeychain}
	default:
		// Backend reachable, no token stored yet — mint one and store it.
		fresh := mintOperatorToken()
		if serr := keychainSet(keychainService, keychainUser, fresh); serr != nil {
			slog.Warn("operator token: keychain store failed, using file fallback", "err", serr)
			break
		}
		return fresh, tokenSource{kind: tokenSourceKeychain}
	}

	tok, src, err := loadOrCreateOperatorTokenFile(operatorTokenFilePath())
	if err != nil {
		slog.Error("operator token: persistence failed, falling back to an ephemeral token", "err", err)
		return mintOperatorToken(), tokenSource{kind: tokenSourceEphemeral}
	}
	return tok, src
}

// keychainLookup reads the operator token from the OS keychain. found is
// false (with a nil err) when the backend is reachable but holds no token
// yet; err is non-nil only when the backend itself is unavailable/failed.
func keychainLookup() (tok string, found bool, err error) {
	v, e := keychainGet(keychainService, keychainUser)
	if e != nil {
		if errors.Is(e, keyring.ErrNotFound) {
			return "", false, nil
		}
		return "", false, e
	}
	v = strings.TrimSpace(v)
	if v == "" {
		// An empty stored value is treated as "not set" so we re-mint.
		return "", false, nil
	}
	return v, true, nil
}

// operatorTokenFilePath is the 0600 file backing the keychain fallback.
// Empty when the home directory cannot be resolved (the caller then
// degrades to an ephemeral token). A var so tests can redirect it at a temp
// dir without touching the real ~/.tclaude.
var operatorTokenFilePath = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tclaude", "operator_token")
}

// loadOrCreateOperatorTokenFile reads the operator token from path, or mints
// and writes a fresh one (0600) when the file is absent or empty. An
// existing file is re-chmodded to 0600 defensively. Returns an error only
// on an unexpected read/write failure (a missing file is not an error).
func loadOrCreateOperatorTokenFile(path string) (string, tokenSource, error) {
	if path == "" {
		return "", tokenSource{}, errors.New("could not resolve operator token file path (no home dir)")
	}
	switch b, err := os.ReadFile(path); {
	case err == nil:
		if tok := strings.TrimSpace(string(b)); tok != "" {
			_ = os.Chmod(path, 0o600)
			return tok, tokenSource{kind: tokenSourceFile, path: path}, nil
		}
		// Empty/whitespace file — re-mint below.
	case !os.IsNotExist(err):
		return "", tokenSource{}, fmt.Errorf("read operator token file %s: %w", path, err)
	}

	tok := mintOperatorToken()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", tokenSource{}, fmt.Errorf("create operator token dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", tokenSource{}, fmt.Errorf("write operator token file %s: %w", path, err)
	}
	return tok, tokenSource{kind: tokenSourceFile, path: path}, nil
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
	return operatorTokenMatches(r.Header.Get(humanTokenHeader))
}

// operatorTokenMatches reports whether got equals the current operator
// token. Fails closed — false when got is blank (after trimming) or
// when no token has been generated yet (e.g. tests, or a startup that
// could not mint one). Constant-time compare so a caller cannot probe
// the token byte-by-byte through timing. This is the shared predicate
// behind both the CLI header path (verifyHumanToken) and the dashboard
// browser-login path (handleDashboardLogin).
func operatorTokenMatches(got string) bool {
	got = strings.TrimSpace(got)
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
// startup banner. It prints a ready-to-paste `export` line when stdout is a
// real terminal.
//
// When stdout is NOT a TTY (the daemon was backgrounded or its output
// redirected, e.g. into ~/.tclaude/output.log) it must never print the
// token — it could land in a log file. For an ephemeral token that means
// the token is unretrievable, so it tells the operator to relaunch attached
// to a terminal. For a PERSISTED token it instead points at where the token
// lives (keychain, or the 0600 file path — never the secret itself), since
// a stable token can be retrieved there and only needs exporting once.
func printOperatorTokenBanner(tok string, src tokenSource) {
	isTTY := false
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		isTTY = true
	}
	if isTTY {
		fmt.Printf("  operator token — the human sets it with:\n")
		fmt.Printf("    export %s=%q\n", humanTokenEnvVar, tok)
		switch src.kind {
		case tokenSourceKeychain:
			fmt.Printf("    (persisted in the OS keychain — stable across restarts, export once)\n")
		case tokenSourceFile:
			fmt.Printf("    (persisted at %s — stable across restarts, export once)\n", src.path)
		}
		return
	}
	switch src.kind {
	case tokenSourceKeychain:
		fmt.Printf("  operator token persisted in the OS keychain — stable across restarts; retrieve it from the keychain\n")
	case tokenSourceFile:
		fmt.Printf("  operator token persisted at %s (0600) — stable across restarts\n", src.path)
	default:
		fmt.Printf("  operator token issued — relaunch agentd attached to a terminal to see it\n")
	}
}
