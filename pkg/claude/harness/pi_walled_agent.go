package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/common"
)

// A pi launch inside tclaude's sandbox does not run against the operator's
// agent directory (the host directory, $PI_CODING_AGENT_DIR or ~/.pi/agent).
// It runs against a walled agent directory tclaude keeps for that host
// directory, under ~/.tclaude/pi-agent, and the host directory is read-only
// inside the wall apart from its session store.
//
// Why: pi runs an api_key credential whose key starts with `!` as a shell
// command, and models.json and settings.json can name commands and code too.
// A writable host directory would let a walled agent plant one for the
// operator's next unsandboxed pi; a read-only one would stop an OAuth login
// from refreshing. The walled directory holds:
//
//   - auth.json: a copy of the host's, which pi may rewrite. Only a refreshed
//     OAuth login travels back to the host (SyncPiWalledAuth): never an API
//     key, never a provider the host did not already log in to.
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

// PiWalledAgentDir is the walled agent directory tclaude keeps for the host
// agent directory host.
func PiWalledAgentDir(host string) (string, error) {
	root := common.TclaudeDir()
	if root == "" {
		return "", errors.New("cannot resolve tclaude's directory")
	}
	sum := sha256.Sum256([]byte(filepath.Clean(host)))
	return filepath.Join(root, "pi-agent", hex.EncodeToString(sum[:8])), nil
}

// PreparePiWalledAgentDir readies the walled agent directory for the ambient
// host agent directory and returns it: a refreshed login merged back to the
// host first, then the credential copy and the links rebuilt from the host.
func PreparePiWalledAgentDir() (string, error) {
	host := PiAgentDir()
	if host == "" {
		return "", errors.New("cannot resolve pi's agent directory")
	}
	walled, err := PiWalledAgentDir(host)
	if err != nil {
		return "", err
	}
	// pi creates both itself on first run; the wall needs them to exist to
	// mount them.
	for _, dir := range []string{host, filepath.Join(host, "sessions"), walled} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("prepare pi's walled agent directory: %w", err)
		}
	}
	if info, err := os.Lstat(walled); err != nil || !info.IsDir() {
		return "", fmt.Errorf("pi's walled agent directory %s is not a directory", walled)
	}
	if err := syncPiWalledAuth(host, walled, true); err != nil {
		return "", err
	}
	if err := linkPiWalledAgentDir(host, walled); err != nil {
		return "", err
	}
	return walled, nil
}

// SyncPiWalledAuth carries refreshed OAuth logins between the ambient host
// agent directory and its walled copy, in whichever direction holds the newer
// login. The daemon runs it periodically so a login refreshed inside the wall
// reaches the host before the host's own refresh token goes stale (a provider
// that rotates refresh tokens would otherwise log the operator out).
func SyncPiWalledAuth() error {
	host := PiAgentDir()
	if host == "" {
		return nil
	}
	walled, err := PiWalledAgentDir(host)
	if err != nil {
		return err
	}
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
// exactly what pi wrote.
type piCredential struct {
	raw     json.RawMessage
	typ     string
	expires int64
}

func readPiCredentials(path string, follow bool) (map[string]piCredential, bool, error) {
	var raw []byte
	var err error
	if follow {
		raw, err = os.ReadFile(path)
	} else {
		raw, err = readNoFollow(path)
	}
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
		if json.Unmarshal(entry, &shape) != nil {
			continue
		}
		credential := piCredential{raw: entry, typ: shape.Type}
		if shape.Type == "oauth" {
			var access, refresh string
			expires, err := shape.Expires.Int64()
			if err != nil || json.Unmarshal(shape.Access, &access) != nil ||
				json.Unmarshal(shape.Refresh, &refresh) != nil {
				// Not a login pi itself would accept.
				continue
			}
			credential.expires = expires
		}
		out[provider] = credential
	}
	return out, true, nil
}

// syncPiWalledAuth merges logins both ways and, with rebuild, then replaces
// the walled copy with the host's credentials.
//
// Toward the host only an OAuth login travels, only for a provider the host
// already has an OAuth login for, and only when it is newer. Everything else
// a walled agent wrote stays in the walled copy, which the next walled launch
// rebuilds.
func syncPiWalledAuth(host, walled string, rebuild bool) error {
	hostPath, walledPath := filepath.Join(host, "auth.json"), filepath.Join(walled, "auth.json")
	return withPiAuthLock(hostPath, func() error {
		return withPiAuthLock(walledPath, func() error {
			hostCreds, hostExists, err := readPiCredentials(hostPath, true)
			if err != nil {
				return fmt.Errorf("read pi's auth.json: %w", err)
			}
			walledCreds, _, err := readPiCredentials(walledPath, false)
			if err != nil {
				// A copy pi could not parse either: nothing to carry back.
				walledCreds = map[string]piCredential{}
			}
			hostChanged, walledChanged := false, false
			for provider, w := range walledCreds {
				h, ok := hostCreds[provider]
				if !ok || h.typ != "oauth" || w.typ != "oauth" {
					continue
				}
				switch {
				case w.expires > h.expires:
					hostCreds[provider] = w
					hostChanged = true
				case h.expires > w.expires:
					walledCreds[provider] = h
					walledChanged = true
				}
			}
			if hostChanged {
				if err := writePiCredentials(hostPath, hostCreds); err != nil {
					return fmt.Errorf("write pi's auth.json: %w", err)
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
		})
	})
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
)

func withPiAuthLock(path string, fn func() error) error {
	lock := path + ".lock"
	deadline := time.Now().Add(piAuthLockWait)
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
			return fmt.Errorf("lock %s: held by another pi", path)
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
