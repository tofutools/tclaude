package workgraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// ── Trust model for external (dir:/git:) template sources ───────────────────
//
// A dir: or git: source is THIRD-PARTY data. Resolution in this file only
// fetches and parses static definition files (flow.mmd / workgraph.yaml /
// nodes/*.yaml) — it NEVER executes anything. Layers that keep that safe:
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
//  3. The clone itself is hardened: url/ref may not begin with '-' (so a crafted
//     ref can't smuggle a git flag like --upload-pack), positional args are
//     fenced with "--", the ext transport (a known git RCE vector) is disabled,
//     and every git invocation runs under a deadline so a hostile/slow server
//     cannot hang the resolver.
//
// Residual, documented: git: subpath traversal is rejected lexically AND a
// symlinked template dir that escapes the clone is rejected (ensureWithin). A
// hostile repo could still ship an individual config file (workgraph.yaml, …) as
// a symlink to an arbitrary local path; LoadDir would then READ that file
// (parsed as YAML, no execution, no network egress at resolve time). Bounded by
// the explicit opt-in + read-only nature; a symlink-safe loader (os.Root) is a
// future hardening if external sourcing sees wide use.
//
// Auth for private repos relies on the user's own git credential helpers / SSH.
// GIT_TERMINAL_PROMPT=0 makes a missing credential fail fast instead of hanging.

const (
	// DefaultGitTTL is how long a cached mutable git ref (branch, tag, or the
	// default branch) is reused before a re-fetch. A full 40-hex commit SHA is
	// immutable and never expires.
	DefaultGitTTL = time.Hour
	// DefaultGitTimeout caps any single git invocation, so a hostile or slow
	// remote cannot hang the resolver indefinitely.
	DefaultGitTimeout = 2 * time.Minute
)

// ResolveOptions tunes resolution of external git: sources. The zero value is
// valid (default TTL, default timeout, default cache dir, no forced refresh).
type ResolveOptions struct {
	Refresh  bool          // force a re-fetch of a mutable git ref even if cached
	TTL      time.Duration // staleness window for mutable git refs (<=0 → DefaultGitTTL)
	Timeout  time.Duration // per-git-invocation deadline (<=0 → DefaultGitTimeout)
	CacheDir string        // git clone cache root ("" → CacheDir())
}

// CacheDir is the root for cached git template clones:
// ~/.tclaude/workgraphs-cache. Returns "" if the config dir is unknown.
func CacheDir() string {
	cd := config.ConfigDir()
	if cd == "" {
		return ""
	}
	return filepath.Join(cd, "workgraphs-cache")
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
		return nil, fmt.Errorf("dir workgraph %q: %s is not a template dir (no workgraph.yaml)", ref, abs)
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

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultGitTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	dest, err := fetchGit(ctx, gs, opts)
	if err != nil {
		return nil, fmt.Errorf("git ref %q: %w", ref, err)
	}
	tmplDir := filepath.Join(dest, filepath.FromSlash(gs.subpath))
	if err := ensureWithin(dest, tmplDir); err != nil {
		return nil, fmt.Errorf("git ref %q: %w", ref, err)
	}
	if !isTemplateDir(tmplDir) {
		return nil, fmt.Errorf("git ref %q: %s is not a template dir (no workgraph.yaml)", ref, displaySub(gs.subpath))
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
//
// url and ref may not begin with '-': git treats a leading-dash argument as a
// flag, so allowing one would let a crafted ref inject options (e.g.
// --upload-pack=<cmd>) into the clone.
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
	gitRef = strings.TrimSpace(gitRef)
	if url == "" {
		return gitSpec{}, fmt.Errorf("git ref %q: empty url", ref)
	}
	if strings.HasPrefix(url, "-") {
		return gitSpec{}, fmt.Errorf("git ref %q: url may not start with '-'", ref)
	}
	if strings.HasPrefix(gitRef, "-") {
		return gitSpec{}, fmt.Errorf("git ref %q: ref may not start with '-'", ref)
	}
	return gitSpec{
		url:     url,
		ref:     gitRef,
		subpath: strings.Trim(subpath, "/"),
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

// checkSubpath rejects a git: subpath that escapes the clone root. It is
// deliberately platform-independent (normalising '\' and rejecting drive
// letters) so a repo authored on one OS can't smuggle a traversal past a
// consumer on another.
func checkSubpath(sub string) error {
	if sub == "" {
		return nil
	}
	norm := strings.ReplaceAll(sub, `\`, "/")
	if strings.HasPrefix(norm, "/") || hasDriveLetter(norm) {
		return fmt.Errorf("subpath %q must be relative", sub)
	}
	clean := path.Clean(norm)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("subpath %q escapes the repository", sub)
	}
	return nil
}

func hasDriveLetter(s string) bool {
	return len(s) >= 2 && s[1] == ':' &&
		((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z'))
}

// ensureWithin rejects a target that, after symlink resolution, escapes root —
// catching a checked-out template dir that is a symlink pointing outside the
// clone. A non-existent target (e.g. a missing template dir) is left to the
// lexical checkSubpath guard + the isTemplateDir check that follow.
func ensureWithin(root, target string) error {
	et, err := filepath.EvalSymlinks(target)
	if err != nil {
		return nil // doesn't exist yet
	}
	er, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil
	}
	rel, err := filepath.Rel(er, et)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("resolved template path escapes the clone (symlink)")
	}
	return nil
}

// fetchGit ensures a clone for gs exists in the cache and returns its directory.
// A fresh ref is cloned; a stale mutable ref is re-cloned; a pinned commit or a
// fresh-enough mutable ref is reused as-is.
func fetchGit(ctx context.Context, gs gitSpec, opts ResolveOptions) (string, error) {
	root := opts.CacheDir
	if root == "" {
		root = CacheDir()
	}
	if root == "" {
		return "", fmt.Errorf("no workgraphs cache dir (home directory not found)")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	dest := filepath.Join(root, cacheKey(gs.url, gs.ref))
	stamp := dest + ".fetched"

	switch {
	case !dirExists(dest):
		slog.Info("workgraph: cloning git template source", "url", gs.url, "ref", refOrDefault(gs.ref), "dest", dest)
		if err := gitClone(ctx, gs, dest); err != nil {
			return "", err
		}
	case shouldRefresh(gs, stamp, opts):
		slog.Info("workgraph: refreshing git template source", "url", gs.url, "ref", refOrDefault(gs.ref), "dest", dest)
		if err := gitClone(ctx, gs, dest); err != nil {
			return "", err
		}
	default:
		slog.Debug("workgraph: using cached git template source", "url", gs.url, "ref", refOrDefault(gs.ref), "dest", dest)
		return dest, nil
	}
	touch(stamp)
	return dest, nil
}

// shouldRefresh decides whether a cached clone must be re-fetched. Only a full
// 40-hex commit SHA is treated as immutable; a branch, tag, or the default
// branch is re-fetched once its cached clone is older than the TTL. (Treating
// abbreviated SHAs / tags as mutable only costs a harmless re-clone to the same
// commit — the safe direction vs. silently serving a stale branch.)
func shouldRefresh(gs gitSpec, stamp string, opts ResolveOptions) bool {
	if opts.Refresh {
		return true
	}
	if isCommitSHA(gs.ref) {
		return false // a pinned full commit SHA is immutable
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

// gitClone clones gs and publishes it at dest atomically: it clones into a
// unique temp dir under the same parent, then renames it into place. A failed
// clone only ever removes its own temp dir, never dest, so two concurrent
// resolves of the same ref can't clobber each other's good clone.
func gitClone(ctx context.Context, gs gitSpec, dest string) error {
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(dest)+".tmp-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(tmp)
		}
	}()

	if err := cloneInto(ctx, gs, tmp); err != nil {
		return err
	}

	// Publish: replace any existing dest with our fresh clone. RemoveAll runs
	// only here, after a *successful* clone — never on the error path — so a
	// failing clone can't delete a sibling's good clone.
	_ = os.RemoveAll(dest)
	if err := os.Rename(tmp, dest); err != nil {
		if dirExists(dest) { // a concurrent resolver published first — use theirs
			_ = os.RemoveAll(tmp)
			published = true
			return nil
		}
		return err
	}
	published = true
	return nil
}

// cloneInto clones gs into the (empty) dir tmp at the requested ref. It tries a
// shallow branch/tag clone first, falling back to a full clone + detached
// checkout for commit SHAs (which --branch cannot take). The ext transport is
// disabled and url is fenced with "--" (see the trust-model note).
func cloneInto(ctx context.Context, gs gitSpec, tmp string) error {
	hard := []string{"-c", "protocol.ext.allow=never"}
	clone := func(extra ...string) []string {
		args := append([]string{}, hard...)
		args = append(args, "clone", "--quiet")
		args = append(args, extra...)
		return append(args, "--", gs.url, tmp)
	}

	if gs.ref == "" {
		return runGit(ctx, "clone", "", clone("--depth", "1")...)
	}
	if err := runGit(ctx, "clone", "", clone("--depth", "1", "--branch", gs.ref)...); err == nil {
		return nil
	}
	// Fallback (commit SHA, or a server that rejected the shallow branch): full
	// clone into a clean tmp, then detached checkout. ref is validated non-dash.
	_ = os.RemoveAll(tmp)
	if err := runGit(ctx, "clone", "", clone()...); err != nil {
		return err
	}
	return runGit(ctx, "checkout", tmp, "-c", "advice.detachedHead=false", "checkout", "--quiet", gs.ref)
}

// runGit runs a git command (in dir, if non-empty) under ctx with interactive
// auth prompts disabled, surfacing stderr (or a timeout) on failure. label is
// used only for error messages.
func runGit(ctx context.Context, label, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("git %s: timed out", label)
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("git %s: %w: %s", label, err, msg)
		}
		return fmt.Errorf("git %s: %w", label, err)
	}
	return nil
}

// cacheKey is a stable, filesystem-safe directory name for a (url, ref) pair.
// Including ref means different refs of one repo are cached independently, so a
// pinned commit stays immutable while a branch refreshes on its own. The url is
// normalised (trailing slash / ".git" stripped) so "…/r" and "…/r.git" share a
// cache rather than cloning twice.
func cacheKey(url, ref string) string {
	norm := strings.TrimSuffix(strings.TrimRight(url, "/"), ".git")
	sum := sha256.Sum256([]byte(norm + "\x00" + ref))
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

// isCommitSHA reports whether ref is a full 40-hex commit SHA. Only a full SHA
// is treated as immutable (see shouldRefresh) — an abbreviated SHA is
// ambiguous with a ref name, so it is left mutable.
func isCommitSHA(ref string) bool {
	if len(ref) != 40 {
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
