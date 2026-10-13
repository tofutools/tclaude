package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/common"
)

// A pi launch inside tclaude's sandbox does not run against the operator's
// agent directory (the host directory, $PI_CODING_AGENT_DIR or ~/.pi/agent).
// It runs against a walled agent directory tclaude keeps for that host
// directory and that conversation, under ~/.tclaude/pi-agent, and the host
// directory is read-only inside the wall apart from its session store.
//
// Why: pi runs an api_key credential whose key starts with `!` as a shell
// command, and models.json and settings.json can name commands and code too.
// A writable host directory would let a walled agent plant one for the
// operator's next unsandboxed pi; a read-only one would stop an OAuth login
// from refreshing. The walled directory holds:
//
//   - auth.json: a copy of the host's, which pi may rewrite. Only a refresh
//     that rotated a reviewed provider's tokens for the same account travels
//     back to the host (SyncPiWalledAuth), and only its tokens and expiry:
//     never an API key, a login the host has none for, or any other field.
//   - a symlink to the host entry for everything else pi reads, so pi sees the
//     operator's settings, models, extensions and sessions, and a write through
//     one lands on the read-only host directory and fails; sessions is the one
//     link whose target the wall leaves writable.
//   - whatever pi writes at run time (tmp/, crashes.json, logs, lock
//     directories), which concerns walled launches only.
//
// The walled directory is rebuilt from the host before every walled launch,
// so whatever a walled agent changed in it does not outlive the next launch.

// piWalledLinks are the host entries a walled directory always links, present
// on the host or not: a dangling link still stops a walled agent from creating
// the file, since the write lands on the read-only host directory.
var piWalledLinks = []string{
	"sessions", "settings.json", "models.json", "models-store.json", "mcp.json",
	"mcp-auth.json", "trust.json", "keybindings.json", "AGENTS.override.md",
	"AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD", "SYSTEM.md", "APPEND_SYSTEM.md",
	"extensions", "skills", "prompts", "themes", "git", "npm", "node_modules", "bin", "tools",
}

// piWalledOwn are the entries a walled directory keeps for itself: its
// credential copy and what pi writes at run time.
var piWalledOwn = map[string]bool{
	"auth.json": true, "oauth.json": true, "tmp": true, "crashes.json": true,
	"pi-debug.log": true, "mcp.log": true,
}

// PiWalledAgentDir is the walled agent directory tclaude keeps for one
// conversation launched against the host agent directory host. Each
// conversation has its own, so no two walled agents ever share a writable
// copy: what one plants there cannot reach another's pi. A blank conversation
// names the directory holding them all, which a launch never runs against.
func PiWalledAgentDir(host, conversation string) (string, error) {
	root := common.TclaudeDir()
	if root == "" {
		return "", errors.New("cannot resolve tclaude's directory")
	}
	hostRoot := filepath.Join(root, "pi-agent", piHostKey(host))
	if conversation == "" {
		return hostRoot, nil
	}
	if !piConversationKey.MatchString(conversation) {
		return "", fmt.Errorf("pi's walled agent directory: invalid conversation id %q", conversation)
	}
	return filepath.Join(hostRoot, conversation), nil
}

func piHostKey(host string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(host)))
	return hex.EncodeToString(sum[:8])
}

// piConversationKey is the shape a conversation id must have to name a
// directory: pi's session ids are UUIDs.
var piConversationKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// PreparePiWalledAgentDir readies the walled agent directory for one
// conversation against the ambient host agent directory and returns it: a
// refreshed login merged back to the host first, then the credential copy and
// the links rebuilt from the host.
func PreparePiWalledAgentDir(conversation string) (string, error) {
	host := PiAgentDir()
	if host == "" {
		return "", errors.New("cannot resolve pi's agent directory")
	}
	if conversation == "" {
		return "", errors.New("pi's walled agent directory needs the launch's conversation id")
	}
	walled, err := PiWalledAgentDir(host, conversation)
	if err != nil {
		return "", err
	}
	// pi creates both host directories itself on first run; the wall needs
	// them to exist to mount them.
	for _, dir := range []string{host, filepath.Join(host, "sessions"), walled} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("prepare pi's walled agent directory: %w", err)
		}
	}
	if info, err := os.Lstat(walled); err != nil || !info.IsDir() {
		return "", fmt.Errorf("pi's walled agent directory %s is not a directory", walled)
	}
	if err := recordPiWalledAgentDir(host, conversation); err != nil {
		return "", err
	}
	if err := syncPiWalledAuth(host, walled, true); err != nil {
		return "", err
	}
	if err := linkPiWalledAgentDir(host, walled); err != nil {
		return "", err
	}
	return walled, nil
}

// SyncPiWalledAuth carries refreshed OAuth logins between every host agent
// directory a walled launch was prepared for and its walled copy. The daemon
// runs it periodically so a login refreshed inside the wall reaches the host
// before the host's own refresh token goes stale (a provider that rotates
// refresh tokens would otherwise log the operator out).
//
// The pairs come from tclaude's private data directory, which walled agents
// cannot see, not from the daemon's own environment: a launch's host
// directory follows the environment of the `session new` that prepared it.
//
// live reports whether a conversation still has a session. A walled
// directory whose conversation has none is synced one last time and removed;
// nil keeps every directory.
func SyncPiWalledAuth(live func(conversation string) bool) error {
	pairs, err := piWalledAgentPairs()
	if err != nil {
		return err
	}
	var errs []error
	for _, pair := range pairs {
		walled, err := PiWalledAgentDir(pair.host, pair.conversation)
		if err != nil {
			continue
		}
		if err := syncPiWalledAuthIfChanged(pair.host, walled); err != nil {
			errs = append(errs, err)
			continue
		}
		if live == nil || live(pair.conversation) || time.Since(pair.recorded) < piWalledAgentGrace {
			continue
		}
		if err := os.RemoveAll(walled); err != nil {
			errs = append(errs, fmt.Errorf("remove pi's walled agent directory: %w", err))
			continue
		}
		_ = os.Remove(pair.record)
		piWalledAuthSeen.Lock()
		delete(piWalledAuthSeen.stamps, walled)
		piWalledAuthSeen.Unlock()
	}
	return errors.Join(errs...)
}

// piWalledAgentGrace keeps a walled directory whose conversation has no
// session yet: a launch prepares it before its session is recorded.
const piWalledAgentGrace = time.Hour

func syncPiWalledAuthIfChanged(host, walled string) error {
	stamp := piAuthStamp(filepath.Join(host, "auth.json")) + "|" + piAuthStamp(filepath.Join(walled, "auth.json"))
	if strings.HasSuffix(stamp, "|") {
		return nil // no walled copy yet
	}
	piWalledAuthSeen.Lock()
	defer piWalledAuthSeen.Unlock()
	if piWalledAuthSeen.stamps[walled] == stamp {
		return nil
	}
	if err := syncPiWalledAuth(host, walled, false); err != nil {
		return err
	}
	if piWalledAuthSeen.stamps == nil {
		piWalledAuthSeen.stamps = map[string]string{}
	}
	piWalledAuthSeen.stamps[walled] = piAuthStamp(filepath.Join(host, "auth.json")) + "|" +
		piAuthStamp(filepath.Join(walled, "auth.json"))
	return nil
}

// piWalledAgentRecords is where tclaude records, per walled directory, the
// host directory it was prepared for: one file per conversation, named
// <host key>-<conversation> and holding the host path.
func piWalledAgentRecords() (string, error) {
	data := common.TclaudeDataDir()
	if data == "" {
		return "", errors.New("cannot resolve tclaude's data directory")
	}
	return filepath.Join(data, "pi-walled-agents"), nil
}

func recordPiWalledAgentDir(host, conversation string) error {
	records, err := piWalledAgentRecords()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(records, 0o700); err != nil {
		return fmt.Errorf("record pi's walled agent directory: %w", err)
	}
	// Rewritten on every launch: its time is when the directory was last
	// prepared.
	record := filepath.Join(records, piHostKey(host)+"-"+conversation)
	if err := os.WriteFile(record, []byte(filepath.Clean(host)), 0o600); err != nil {
		return fmt.Errorf("record pi's walled agent directory: %w", err)
	}
	return nil
}

type piWalledAgentPair struct {
	host, conversation, record string
	recorded                   time.Time
}

func piWalledAgentPairs() ([]piWalledAgentPair, error) {
	records, err := piWalledAgentRecords()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(records)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pi's walled agent records: %w", err)
	}
	var pairs []piWalledAgentPair
	for _, entry := range entries {
		key, conversation, ok := strings.Cut(entry.Name(), "-")
		info, infoErr := entry.Info()
		if !ok || infoErr != nil || !info.Mode().IsRegular() {
			continue
		}
		record := filepath.Join(records, entry.Name())
		raw, err := os.ReadFile(record)
		host := string(raw)
		if err != nil || !filepath.IsAbs(host) || piHostKey(host) != key || !piConversationKey.MatchString(conversation) {
			continue
		}
		pairs = append(pairs, piWalledAgentPair{host: host, conversation: conversation, record: record, recorded: info.ModTime()})
	}
	return pairs, nil
}

// piWalledAuthSeen remembers the files a sync last settled, so a periodic
// sync with nothing new does not take pi's lock.
var piWalledAuthSeen struct {
	sync.Mutex
	stamps map[string]string
}

func piAuthStamp(path string) string {
	info, err := os.Lstat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
}

// piCredential is one auth.json entry, kept raw so a merge writes back
// exactly what pi wrote. An entry tclaude cannot read is kept too, with no
// type, so a merge never touches it and never drops it.
type piCredential struct {
	raw             json.RawMessage
	typ             string
	access, refresh string
	expires         int64
}

// piAuthMaxBytes bounds what a sync reads: an auth.json holds a handful of
// logins, and a walled agent controls its copy.
const piAuthMaxBytes = 1 << 20

func readPiCredentials(path string, follow bool) (map[string]piCredential, bool, error) {
	raw, err := readRegularFile(path, follow, piAuthMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]piCredential{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var entries map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, true, err
		}
	}
	out := make(map[string]piCredential, len(entries))
	for provider, entry := range entries {
		var shape struct {
			Type    string          `json:"type"`
			Access  json.RawMessage `json:"access"`
			Refresh json.RawMessage `json:"refresh"`
			Expires json.Number     `json:"expires"`
		}
		credential := piCredential{raw: entry}
		if json.Unmarshal(entry, &shape) != nil {
			out[provider] = credential
			continue
		}
		credential.typ = shape.Type
		if shape.Type == "oauth" {
			var access, refresh string
			expires, err := shape.Expires.Int64()
			if err != nil || json.Unmarshal(shape.Access, &access) != nil ||
				json.Unmarshal(shape.Refresh, &refresh) != nil {
				// Not a login pi itself would accept: kept as it is.
				out[provider] = piCredential{raw: entry}
				continue
			}
			credential.access, credential.refresh, credential.expires = access, refresh, expires
		}
		out[provider] = credential
	}
	return out, true, nil
}

// syncPiWalledAuth merges logins both ways and, with rebuild, then replaces
// the walled copy with the host's credentials.
//
// Toward the walled copy, a newer host login replaces the walled one. Toward
// the host, only what piRefreshedLogin accepts travels. Everything else a
// walled agent wrote stays in the walled copy, which the next walled launch
// rebuilds.
func syncPiWalledAuth(host, walled string, rebuild bool) error {
	hostPath, walledPath := filepath.Join(host, "auth.json"), filepath.Join(walled, "auth.json")
	// The daemon's periodic sync waits only briefly for a walled lock, which a
	// walled agent may hold for as long as it likes.
	walledWait := piAuthLockWait
	if !rebuild {
		walledWait = piWalledAuthLockWait
	}
	return withPiAuthLock(hostPath, piAuthLockWait, func() error {
		err := withPiAuthLock(walledPath, walledWait, func() error {
			return mergePiWalledAuth(hostPath, walledPath, rebuild, true)
		})
		if errors.Is(err, errPiAuthLockHeld) && rebuild {
			// A walled agent can hold its own copy's lock forever. The
			// rebuild replaces that copy by rename regardless, so it goes
			// ahead without carrying anything back.
			return mergePiWalledAuth(hostPath, walledPath, true, false)
		}
		return err
	})
}

func mergePiWalledAuth(hostPath, walledPath string, rebuild, carryBack bool) error {
	hostCreds, hostExists, err := readPiCredentials(hostPath, true)
	if err != nil {
		return fmt.Errorf("read pi's auth.json: %w", err)
	}
	walledCreds := map[string]piCredential{}
	if carryBack {
		if walledCreds, _, err = readPiCredentials(walledPath, false); err != nil {
			// A copy pi could not parse either: nothing to carry back.
			walledCreds = map[string]piCredential{}
		}
	}
	hostChanged, walledChanged := false, false
	for provider, w := range walledCreds {
		h, ok := hostCreds[provider]
		if !ok || h.typ != "oauth" || w.typ != "oauth" {
			continue
		}
		if merged, ok := piRefreshedLogin(provider, h, w, time.Now()); ok {
			hostCreds[provider] = merged
			hostChanged = true
		} else if h.expires > w.expires {
			walledCreds[provider] = h
			walledChanged = true
		}
	}
	if hostChanged {
		// The operator's auth.json may be a link (a dotfiles checkout, say):
		// the write goes to its target, so the link stays a link. One that
		// does not resolve keeps the host login as it is.
		if target, err := filepath.EvalSymlinks(hostPath); err == nil {
			if err := writePiCredentials(target, hostCreds); err != nil {
				return fmt.Errorf("write pi's auth.json: %w", err)
			}
		}
	}
	if rebuild {
		walledCreds, walledChanged = hostCreds, true
		if !hostExists {
			walledCreds = map[string]piCredential{}
		}
	}
	if walledChanged {
		if err := writePiCredentials(walledPath, walledCreds); err != nil {
			return fmt.Errorf("write pi's walled auth.json: %w", err)
		}
	}
	return nil
}

// piLoginIdentity names, for each provider whose refreshed login may travel
// back to the host, the account a login's access token belongs to. A walled
// agent can write anything into its copy, and pi trusts more of a login than
// its tokens (Copilot's API host comes from its token and enterpriseUrl), so
// only a reviewed provider's tokens travel, and only for the host's account.
//
// Providers left out keep the host's login; one whose refresh tokens rotate
// may then have to log in again outside the wall. Copilot needs no entry: its
// refresh token is a GitHub token that does not rotate.
var piLoginIdentity = map[string]func(access string) string{
	"openai-codex": piCodexAccountID,
}

// piRefreshedLoginMaxLifetime caps the expiry a carried-back login is given,
// so a forged far-future expiry cannot pin it in place of the host's own
// refreshes.
const piRefreshedLoginMaxLifetime = 60 * 24 * time.Hour

// piRefreshedLogin returns the host login updated with the walled refresh of
// it, when the walled one is a genuine refresh: a reviewed provider, newer,
// a rotated refresh token, and an access token for the same account. Only the
// access token, refresh token and expiry change; every other field stays the
// host's.
func piRefreshedLogin(provider string, host, walled piCredential, now time.Time) (piCredential, bool) {
	identity := piLoginIdentity[provider]
	if identity == nil || walled.expires <= host.expires || walled.refresh == host.refresh ||
		walled.access == "" || walled.refresh == "" {
		return piCredential{}, false
	}
	account := identity(host.access)
	if account == "" || identity(walled.access) != account {
		return piCredential{}, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(host.raw, &fields) != nil {
		return piCredential{}, false
	}
	expires := min(walled.expires, now.Add(piRefreshedLoginMaxLifetime).UnixMilli())
	for name, value := range map[string]any{"access": walled.access, "refresh": walled.refresh, "expires": expires} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return piCredential{}, false
		}
		fields[name] = encoded
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return piCredential{}, false
	}
	return piCredential{raw: raw, typ: host.typ, access: walled.access, refresh: walled.refresh, expires: expires}, true
}

// piCodexAccountID reads the ChatGPT account an openai-codex access token
// names, as pi does: the unverified JWT payload's
// "https://api.openai.com/auth".chatgpt_account_id.
func piCodexAccountID(access string) string {
	parts := strings.Split(access, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Auth.AccountID
}

// writePiCredentials replaces path atomically, mode 0600 as pi writes it. A
// rename also replaces a symlink a walled agent may have put in its place.
func writePiCredentials(path string, creds map[string]piCredential) error {
	out := make(map[string]json.RawMessage, len(creds))
	for provider, credential := range creds {
		out[provider] = credential.raw
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".auth-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// pi's credential lock (proper-lockfile): a directory beside the file, held
// briefly, and stale once its mtime is older than piAuthLockStale.
const (
	piAuthLockStale = 10 * time.Second
	piAuthLockWait  = 5 * time.Second
	// piWalledAuthLockWait is how long the periodic sync waits for a walled
	// copy's lock before trying again on its next round.
	piWalledAuthLockWait = 250 * time.Millisecond
)

var errPiAuthLockHeld = errors.New("held by another pi")

func withPiAuthLock(path string, wait time.Duration, fn func() error) error {
	lock := path + ".lock"
	deadline := time.Now().Add(wait)
	for {
		err := os.Mkdir(lock, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("lock %s: %w", path, err)
		}
		if info, statErr := os.Stat(lock); statErr == nil && time.Since(info.ModTime()) > piAuthLockStale {
			_ = os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("lock %s: %w", path, errPiAuthLockHeld)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer func() { _ = os.Remove(lock) }()
	return fn()
}

// linkPiWalledAgentDir points the walled directory at the host entries: every
// host entry pi reads, plus piWalledLinks whether or not the host has them.
// A link a walled agent replaced or added is put back or removed.
func linkPiWalledAgentDir(host, walled string) error {
	want := map[string]bool{}
	for _, name := range piWalledLinks {
		want[name] = true
	}
	hostEntries, err := os.ReadDir(host)
	if err != nil {
		return fmt.Errorf("read pi's agent directory: %w", err)
	}
	for _, entry := range hostEntries {
		name := entry.Name()
		if piWalledOwn[name] || strings.HasSuffix(name, ".lock") || strings.HasPrefix(name, ".auth-") {
			continue
		}
		want[name] = true
	}
	walledEntries, err := os.ReadDir(walled)
	if err != nil {
		return fmt.Errorf("read pi's walled agent directory: %w", err)
	}
	for _, entry := range walledEntries {
		name := entry.Name()
		if !want[name] && entry.Type()&os.ModeSymlink != 0 {
			if err := os.Remove(filepath.Join(walled, name)); err != nil {
				return fmt.Errorf("prune pi's walled agent directory: %w", err)
			}
		}
	}
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		link, target := filepath.Join(walled, name), filepath.Join(host, name)
		if current, err := os.Readlink(link); err == nil && current == target {
			continue
		}
		if err := os.RemoveAll(link); err != nil {
			return fmt.Errorf("relink %s: %w", link, err)
		}
		if err := os.Symlink(target, link); err != nil {
			return fmt.Errorf("link %s: %w", link, err)
		}
	}
	return nil
}
