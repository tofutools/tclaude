package statusbar

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// --- the pane-local render cache ---
//
// The change gate and the read TTL both need to remember what the last
// render did, and a statusline render is a fresh short-lived process
// every time — so the memory has to live outside it.
//
// It lives in the pane's own /tmp, and that placement is the whole design
// rather than a convenience. Inside `tclaude-layer` the sandbox gives each
// launch a private tmpfs at /tmp: the cache is therefore invisible to
// every other agent, unreachable from the host, and gone the moment the
// pane exits — its lifetime is correct BY CONSTRUCTION rather than by a
// sweeper that has to be right. It also cannot outlive a relaunch, so a
// resumed agent starts by sending its first render rather than trusting
// something a previous incarnation left behind.
//
// Nothing here is authoritative. The cache holds cosmetic read results
// and a digest of what was last sent; every decision that matters — which
// row a render may write — is made daemon-side from process ancestry on
// every single request. A caller that tampered with this file could at
// worst make its OWN bar stale, or make itself re-send a render the
// daemon will authorise exactly as it authorised the last one.

// renderCache is what one pane remembers between renders.
type renderCache struct {
	// Digest identifies the last payload whose writes the daemon
	// accepted. A render matching it records nothing.
	Digest string `json:"digest"`

	// ReadsAt stamps the cosmetic reads below.
	ReadsAt time.Time `json:"reads_at"`

	// Reads is the daemon's last answer.
	Reads BrokeredRenderResponse `json:"reads"`

	// FailedAt stamps the last round trip that did not complete, and is
	// the backoff the digest cannot provide.
	//
	// A failed attempt deliberately leaves Digest alone, so the write
	// stays owed — but that also means the change gate still reads as
	// "changed" and would retry on the very next render. For an agent the
	// daemon can never place, that is a socket connect and a refusal
	// several times a second for the rest of its life. This suppresses
	// the attempt, not the obligation: once it lapses the write is
	// re-sent, because Digest still does not match.
	FailedAt time.Time `json:"failed_at,omitempty"`
}

// renderCacheDir is the directory the pane-local caches live in.
//
// It is the literal "/tmp" rather than os.TempDir(), and that is the
// point: the sandbox mounts a private tmpfs at /tmp specifically, while
// os.TempDir() honours $TMPDIR — which the launch inherits from the
// operator's shell and does not clear. An operator with TMPDIR set to a
// directory under $HOME would silently move these files somewhere the
// launch contract binds writable, where they are host-visible and outlive
// the pane. Nothing here is secret or authoritative, so that would be a
// staleness bug rather than a leak, but the header above makes an
// argument from the placement and the code should be the placement it
// argues from.
//
// Overridable only from tests.
var renderCacheDir = "/tmp"

// renderCachePath names this pane's cache file. It is keyed by session so
// that a launch mode where /tmp is NOT private (anything outside the
// sandbox that ever reaches this path) still cannot cross agents.
func renderCachePath(sessionID string) string {
	return filepath.Join(renderCacheDir, "tclaude-statusline-"+sessionKeyHash(sessionID)+".json")
}

// renderRefusalStampPath names the pane-local mtime stamp that
// rate-limits the refusal log across render processes.
func renderRefusalStampPath(sessionID string) string {
	return filepath.Join(renderCacheDir, "tclaude-statusline-refused-"+sessionKeyHash(sessionID))
}

func sessionKeyHash(sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:8])
}

func loadRenderCache(sessionID string) *renderCache {
	data, err := os.ReadFile(renderCachePath(sessionID))
	if err != nil {
		return nil
	}
	var c renderCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	// A cache stamped in the future is a clock change, not a fresh read.
	if c.ReadsAt.After(time.Now().Add(renderReadTTL)) {
		return nil
	}
	return &c
}

// saveRenderCache writes the cache, ignoring failures: an unwritable
// cache costs a round trip per render and nothing else, which is exactly
// the behaviour of not having one.
func saveRenderCache(sessionID string, c renderCache) {
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	path := renderCachePath(sessionID)
	// Write-then-rename so a render that dies mid-write cannot leave a
	// truncated cache that the next render reads as "nothing changed".
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// --- the git snapshot cache, pane-local under brokering ---
//
// The git snapshot is the one piece of the statusline the sandbox can
// still gather for itself: git and gh both run inside the wall, on a
// worktree the launch contract binds. Only its CACHE lived in the
// database. Brokering that would put a round trip in front of data the
// pane already has, so under the layer the same 15-second cache simply
// moves into the pane's /tmp alongside the render cache.

func gitCachePath(key string) string {
	return filepath.Join(renderCacheDir, "tclaude-gitcache-"+key+".json")
}

func loadLocalGitCache(key string) *GitSnapshot {
	data, err := os.ReadFile(gitCachePath(key))
	if err != nil {
		return nil
	}
	var cached GitSnapshot
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil
	}
	if time.Since(cached.FetchedAt) > gitCacheTTL {
		return nil
	}
	return &cached
}

func saveLocalGitCache(key string, g *GitSnapshot) {
	data, err := json.Marshal(g)
	if err != nil {
		return
	}
	_ = os.WriteFile(gitCachePath(key), data, 0600)
}
