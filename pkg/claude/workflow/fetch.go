package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// ── Trust model for external (dir:/git:) template sources ───────────────────
//
// A dir: or git: source is THIRD-PARTY data. Resolution in this file only
// fetches and parses static definition files (flow.mmd / workflow.yaml /
// nodes/*.yaml) — it NEVER executes anything. Two layers keep that safe:
//
//  1. Resolution is explicit. Nothing scans for or auto-resolves external refs;
//     a caller (CLI/dashboard) must pass one in, which is the operator's opt-in.
//     Callers should surface the source url + ref before resolving, and on the
//     dashboard while an instance runs.
//  2. Execution is gated downstream. Source.IsExternal() marks dir:/git:
//     templates so the execution engine + node approval gates (JOH-17) require
//     confirmation before running an externally-sourced tool/program node. This
//     package introduces that seam; it does not run nodes. Until the gate
//     exists the engine should leave an external tool/program node awaiting
//     (human-gated) rather than auto-running third-party commands.
//
// Path traversal: a git: subpath that climbs out of the clone is rejected
// (checkSubpath) so a malicious ref cannot point the loader at arbitrary files.
//
// Auth for private repos relies on the user's own git credential helpers / SSH.
// GIT_TERMINAL_PROMPT=0 makes a missing credential fail fast instead of hanging
// on an interactive prompt.

// DefaultGitTTL is how long a cached mutable git ref (branch, tag, or the
// default branch) is reused before a re-fetch. Pinned commit SHAs are immutable
// and never expire.
const DefaultGitTTL = time.Hour

// ResolveOptions tunes resolution of external git: sources. The zero value is
// valid (default TTL, default cache dir, no forced refresh).
type ResolveOptions struct {
	Refresh  bool          // force a re-fetch of a mutable git ref even if cached
	TTL      time.Duration // staleness window for mutable git refs (<=0 → DefaultGitTTL)
	CacheDir string        // git clone cache root ("" → CacheDir())
}

// CacheDir is the root for cached git template clones:
// ~/.tclaude/workflows-cache. Returns "" if the config dir is unknown.
func CacheDir() string {
	cd := config.ConfigDir()
	if cd == "" {
		return ""
	}
	return filepath.Join(cd, "workflows-cache")
}

// resolveDir loads a template from a plain directory (dir:<path>). The path may
// be absolute or relative to the current working directory.
func resolveDir(spec, ref string) (*Template, error) {
	p := strings.TrimSpace(spec)
	if p == "" {
		return nil, fmt.Errorf("cannot resolve %q: empty directory path", ref)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve %q: %w", ref, err)
	}
	if !isTemplateDir(abs) {
		return nil, fmt.Errorf("dir workflow %q: %s is not a template dir (no workflow.yaml)", ref, abs)
	}
	return LoadDir(abs, ref, SourceDir)
}

// resolveGit fetches (or reuses a cached clone of) a git repo and loads the
// template at the requested subpath.
func resolveGit(ref string, opts ResolveOptions) (*Template, error) {
	gs, err := parseGitSpec(ref)
	if err != nil {
		return nil, err
	}
	if err := checkSubpath(gs.subpath); err != nil {
		return nil, fmt.Errorf("git ref %q: %w", ref, err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil, fmt.Errorf("git ref %q: git executable not found in PATH: %w", ref, err)
	}
	dest, err := fetchGit(gs, opts)
	if err != nil {
		return nil, fmt.Errorf("git ref %q: %w", ref, err)
	}
	tmplDir := filepath.Join(dest, filepath.FromSlash(gs.subpath))
	if !isTemplateDir(tmplDir) {
		return nil, fmt.Errorf("git ref %q: %s is not a template dir (no workflow.yaml)", ref, displaySub(gs.subpath))
	}
	return LoadDir(tmplDir, ref, SourceGit)
}

// gitSpec is a parsed git: reference.
type gitSpec struct {
	url     string // clone url (https / ssh / scp-like / local path)
	ref     string // branch, tag, or commit; "" → remote default branch
	subpath string // template dir within the repo; "" → repo root
}

// parseGitSpec parses "git:<url>[@<ref>][#<path>]".
//
// The grammar is ambiguous: '@' appears both in scp-style ssh urls
// (git@host:org/repo) and in https userinfo (https://user@host/…) AND as the
// ref delimiter, while a branch ref may itself contain '/'. We disambiguate by
// only treating an '@' as the ref delimiter when it falls in the url's PATH
// region — past scheme+authority for "scheme://…" urls, or past the first ':'
// for scp-style urls. The last such '@' wins.
func parseGitSpec(ref string) (gitSpec, error) {
	spec := strings.TrimPrefix(ref, string(SourceGit)+":")
	urlAndRef, subpath, _ := strings.Cut(spec, "#")
	start := refSearchStart(urlAndRef)
	url, gitRef := urlAndRef, ""
	if at := strings.LastIndexByte(urlAndRef[start:], '@'); at >= 0 {
		idx := start + at
		url, gitRef = urlAndRef[:idx], urlAndRef[idx+1:]
	}
	url = strings.TrimSpace(url)
	if url == "" {
		return gitSpec{}, fmt.Errorf("git ref %q: empty url", ref)
	}
	return gitSpec{
		url:     url,
		ref:     strings.TrimSpace(gitRef),
		subpath: strings.Trim(strings.TrimSpace(subpath), "/"),
	}, nil
}

// refSearchStart returns the index in a git url at which the path portion
// begins, so an '@' before it (scheme userinfo, scp user) is not mistaken for
// the ref delimiter.
func refSearchStart(u string) int {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := i + 3
		if j := strings.IndexByte(u[rest:], '/'); j >= 0 {
			return rest + j // first '/' after the authority
		}
		return len(u) // scheme://authority with no path
	}
	if j := strings.IndexByte(u, ':'); j >= 0 {
		return j + 1 // scp-style user@host:path
	}
	return 0 // local path / bare
}

// checkSubpath rejects a git: subpath that escapes the clone root.
func checkSubpath(sub string) error {
	if sub == "" {
		return nil
	}
	clean := filepath.Clean(filepath.FromSlash(sub))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("subpath %q escapes the repository", sub)
	}
	return nil
}

// fetchGit ensures a clone for gs exists in the cache and returns its directory.
// A fresh ref is cloned; a stale mutable ref is re-cloned; a pinned commit or a
// fresh-enough mutable ref is reused as-is.
func fetchGit(gs gitSpec, opts ResolveOptions) (string, error) {
	root := opts.CacheDir
	if root == "" {
		root = CacheDir()
	}
	if root == "" {
		return "", fmt.Errorf("no workflows cache dir (home directory not found)")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	dest := filepath.Join(root, cacheKey(gs.url, gs.ref))
	stamp := dest + ".fetched"

	switch {
	case !dirExists(dest):
		slog.Info("workflow: cloning git template source", "url", gs.url, "ref", refOrDefault(gs.ref), "dest", dest)
		if err := gitClone(gs, dest); err != nil {
			_ = os.RemoveAll(dest) // don't leave a half-clone behind
			return "", err
		}
	case shouldRefresh(gs, stamp, opts):
		slog.Info("workflow: refreshing git template source", "url", gs.url, "ref", refOrDefault(gs.ref), "dest", dest)
		if err := os.RemoveAll(dest); err != nil {
			return "", fmt.Errorf("clear stale cache: %w", err)
		}
		if err := gitClone(gs, dest); err != nil {
			_ = os.RemoveAll(dest)
			return "", err
		}
	default:
		slog.Debug("workflow: using cached git template source", "url", gs.url, "ref", refOrDefault(gs.ref), "dest", dest)
		return dest, nil
	}
	touch(stamp)
	return dest, nil
}

// shouldRefresh decides whether a cached clone must be re-fetched.
func shouldRefresh(gs gitSpec, stamp string, opts ResolveOptions) bool {
	if opts.Refresh {
		return true
	}
	if isCommitSHA(gs.ref) {
		return false // a pinned commit is immutable
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultGitTTL
	}
	info, err := os.Stat(stamp)
	if err != nil {
		return true // no stamp → treat as stale
	}
	return time.Since(info.ModTime()) > ttl
}

// gitClone clones gs into dest at the requested ref. It tries a shallow
// branch/tag clone first, falling back to a full clone + checkout for commit
// SHAs (which --branch cannot take).
func gitClone(gs gitSpec, dest string) error {
	if gs.ref == "" {
		return runGit("", "clone", "--quiet", "--depth", "1", gs.url, dest)
	}
	if err := runGit("", "clone", "--quiet", "--depth", "1", "--branch", gs.ref, gs.url, dest); err == nil {
		return nil
	}
	// Fallback: a commit SHA (or a server that rejected the shallow branch) →
	// full clone, then detached checkout.
	_ = os.RemoveAll(dest)
	if err := runGit("", "clone", "--quiet", gs.url, dest); err != nil {
		return err
	}
	return runGit(dest, "-c", "advice.detachedHead=false", "checkout", "--quiet", gs.ref)
}

// runGit runs a git command (in dir, if non-empty) with interactive auth
// prompts disabled, surfacing stderr on failure.
func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("git %s: %w: %s", args[0], err, msg)
		}
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}

// cacheKey is a stable, filesystem-safe directory name for a (url, ref) pair.
// Including ref means different refs of one repo are cached independently, so a
// pinned commit/tag stays immutable while a branch refreshes on its own.
func cacheKey(url, ref string) string {
	sum := sha256.Sum256([]byte(url + "\x00" + ref))
	hash := hex.EncodeToString(sum[:])[:16]
	if base := sanitizeForFS(repoBase(url)); base != "" {
		return base + "-" + hash
	}
	return hash
}

// repoBase returns the last path segment of a git url, sans trailing slash/.git.
func repoBase(url string) string {
	s := strings.TrimSuffix(strings.TrimRight(url, "/"), ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// sanitizeForFS keeps only filesystem-safe characters, capped to a short length.
func sanitizeForFS(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// isCommitSHA reports whether ref looks like an (abbreviated) commit SHA, i.e.
// 7–40 hex digits. Such a ref is treated as immutable.
func isCommitSHA(ref string) bool {
	if len(ref) < 7 || len(ref) > 40 {
		return false
	}
	for _, r := range ref {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// touch creates or updates the mtime of a marker file (best-effort).
func touch(path string) {
	if f, err := os.Create(path); err == nil {
		_ = f.Close()
	}
}

func refOrDefault(ref string) string {
	if ref == "" {
		return "(default branch)"
	}
	return ref
}

func displaySub(sub string) string {
	if sub == "" {
		return "(repo root)"
	}
	return sub
}
