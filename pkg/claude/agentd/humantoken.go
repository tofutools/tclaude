package agentd

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
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
// Reached by both the ephemeral path (via generateOperatorToken) and the
// persisted path (resolveOperatorToken), so the verifier/banner are agnostic
// to how the token was sourced.
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
// that survives restarts. The daemon stores it in a 0600 file under
// ~/.tclaude/data by default. The OS keychain is a separate explicit opt-in
// (agent.persist_operator_token_keychain /
// `--persist-operator-token-keychain`) because keychain access is not a
// portable agent-sandbox boundary.
//
// STORAGE THREAT MODEL — the private file keeps the same boundary as the
// in-memory token: the agent sandbox denies reads to ~/.tclaude/data, so a
// sandboxed agent cannot read the file;
// a non-sandboxed same-uid process could, but it can already read the
// human's /proc/<pid>/environ and mutate ~/.tclaude state, so the token was
// never a boundary against it (see the package-level note above). A keychain
// may add at-rest protection, but tclaude does not claim that its platform
// IPC/access policy excludes agents.

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

// shouldPersistOperatorTokenKeychain reports whether the human explicitly
// selected keychain-backed persistence. This option implies persistence at
// the serve call site; it never silently follows keychain availability.
func shouldPersistOperatorTokenKeychain(flagSet bool, cfg *config.Config) bool {
	if flagSet {
		return true
	}
	return cfg != nil && cfg.Agent != nil && cfg.Agent.PersistOperatorTokenKeychain
}

// resolveOperatorTokenPersistence applies the two independent flag/config OR
// rules and the one implication: selecting the keychain also enables
// persistence. When both stores are selected, the explicit keychain choice
// wins.
func resolveOperatorTokenPersistence(persistFlag, keychainFlag bool, cfg *config.Config) (persist, useKeychain bool) {
	useKeychain = shouldPersistOperatorTokenKeychain(keychainFlag, cfg)
	persist = shouldPersistOperatorToken(persistFlag, cfg) || useKeychain
	return persist, useKeychain
}

// resolveOperatorToken sources the operator token for this daemon lifetime
// and installs it as the live token. persist=false keeps the historical
// behaviour (a fresh in-memory token); persist=true loads or creates a
// stable one in the selected backend. Returns the token and where it came
// from. The stores are independent: selecting one never reads or writes the
// other.
func resolveOperatorToken(persist, useKeychain bool) (string, tokenSource) {
	if !persist {
		return generateOperatorToken(), tokenSource{kind: tokenSourceEphemeral}
	}
	var tok string
	var src tokenSource
	if useKeychain {
		tok, src = loadOrCreateOperatorTokenKeychain()
	} else {
		tok, src = loadOrCreateOperatorTokenFileBacked()
	}
	setOperatorToken(tok)
	return tok, src
}

// loadOrCreateOperatorTokenKeychain reads or creates the explicitly selected
// keychain token. Failure degrades to an ephemeral token rather than silently
// writing the private file or leaving the daemon without a credential.
func loadOrCreateOperatorTokenKeychain() (string, tokenSource) {
	switch tok, found, err := keychainLookup(); {
	case err != nil:
		slog.Error("operator token: explicitly selected OS keychain is unavailable; using an ephemeral token", "err", err)
		return mintOperatorToken(), tokenSource{kind: tokenSourceEphemeral}
	case found:
		return tok, tokenSource{kind: tokenSourceKeychain}
	default:
		seed := mintOperatorToken()
		if serr := keychainSet(keychainService, keychainUser, seed); serr != nil {
			slog.Error("operator token: explicitly selected OS keychain store failed; using an ephemeral token", "err", serr)
			return seed, tokenSource{kind: tokenSourceEphemeral}
		}
		return seed, tokenSource{kind: tokenSourceKeychain}
	}
}

// loadOrCreateOperatorTokenFileBacked reads or creates the default persistent
// token file. Failure degrades to an ephemeral token rather than leaving the
// daemon without a credential.
func loadOrCreateOperatorTokenFileBacked() (string, tokenSource) {
	tok, src, err := loadOrCreateOperatorTokenFile(operatorTokenFilePath())
	if err != nil {
		slog.Error("operator token: file persistence failed, using an ephemeral token", "err", err)
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

// operatorTokenFilePath is the default 0600 persistent-token file.
// Empty when the home directory cannot be resolved (the caller then
// degrades to an ephemeral token). A var so tests can redirect it at a temp
// dir without touching the real ~/.tclaude.
var operatorTokenFilePath = func() string {
	dataDir := config.DataDir()
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "operator_token")
}

// loadOrCreateOperatorTokenFile reads the operator token from path, or mints
// and writes a fresh one when the file is absent or empty. The secret is
// always protected by the file's own 0600 mode — enforced on every path:
// a non-empty existing file is re-chmodded defensively on read, and the
// mint-and-write path forces 0600 after the write (os.WriteFile does not
// re-mode an existing file). The containing ~/.tclaude dir is created 0700
// when missing, else left as-is (it is shared with the db / config / logs,
// typically already 0755 — the 0600 file is the boundary, not the dir).
// Returns an error only on an unexpected read/write failure (a missing file
// is not an error).
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
	// os.WriteFile applies its 0600 perm only when CREATING the file — it
	// truncates an existing one without changing its mode. So a pre-existing
	// empty/loose-perm file (e.g. 0644) would otherwise keep its loose mode
	// after we write the secret into it. Force 0600 unconditionally.
	if err := os.Chmod(path, 0o600); err != nil {
		return "", tokenSource{}, fmt.Errorf("chmod operator token file %s: %w", path, err)
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
// printOperatorTokenBanner is the printing form; noPrint (the
// --no-print-human-token flag) suppresses it entirely — the token is still
// minted and honored, only the banner is silenced.
func printOperatorTokenBanner(tok string, src tokenSource, noPrint bool) {
	isTTY := false
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		isTTY = true
	}
	maybeWriteOperatorTokenBanner(os.Stdout, tok, src, isTTY, noPrint)
}

// maybeWriteOperatorTokenBanner writes the banner unless noPrint is set. Split
// from writeOperatorTokenBanner so the --no-print-human-token gate is
// unit-testable without a real terminal.
func maybeWriteOperatorTokenBanner(w io.Writer, tok string, src tokenSource, isTTY, noPrint bool) {
	if noPrint {
		return
	}
	writeOperatorTokenBanner(w, tok, src, isTTY)
}

// writeOperatorTokenBanner renders the banner to w. The secret (tok) is
// emitted ONLY when isTTY is true; every non-TTY branch prints just the
// location (keychain / file path) or a relaunch hint, never the token. Split
// out from printOperatorTokenBanner so this secret-gating is unit-testable
// without a real terminal.
func writeOperatorTokenBanner(w io.Writer, tok string, src tokenSource, isTTY bool) {
	if isTTY {
		fmt.Fprintf(w, "  operator token — the human sets it with:\n")
		fmt.Fprintf(w, "    export %s=%q\n", humanTokenEnvVar, tok)
		switch src.kind {
		case tokenSourceKeychain:
			fmt.Fprintf(w, "    (persisted in the OS keychain — stable across restarts, export once)\n")
		case tokenSourceFile:
			fmt.Fprintf(w, "    (persisted at %s — stable across restarts, export once)\n", src.path)
		}
		return
	}
	switch src.kind {
	case tokenSourceKeychain:
		fmt.Fprintf(w, "  operator token persisted in the OS keychain — stable across restarts; retrieve it from the keychain\n")
	case tokenSourceFile:
		fmt.Fprintf(w, "  operator token persisted at %s (0600) — stable across restarts\n", src.path)
	default:
		fmt.Fprintf(w, "  operator token issued — relaunch agentd attached to a terminal to see it\n")
	}
}
