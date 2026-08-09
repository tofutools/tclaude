package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// githubproxy.go is the daemon half of `tclaude proxy github` — pull-request
// and issue operations performed with agentd's own `gh` credentials on behalf
// of an agent that has been sandboxed away from ~/.config/gh.
//
// It reuses the git proxy's gates wholesale: the repository still comes from
// the agent's daemon-recorded launch directory, and the GitHub repo it acts on
// is DERIVED from that repository's validated, allow-listed remote. There is
// no --repo parameter, and there is no `gh` passthrough. An agent can only
// reach the forge repository its own checkout already points at.
//
// Three things are specific to `gh` and worth stating:
//
//  1. `gh` is run in a NEUTRAL directory, never the agent's repository, and
//     always with an explicit `--repo <owner>/<repo>`. `gh` would otherwise
//     discover the repository by reading .git/config — a file the agent can
//     write — which would defeat the remote allow-list entirely.
//  2. Free text (a PR title, a comment body) is passed through a 0600 file in
//     the daemon's private tree via `--body-file`, never through argv. argv is
//     world-readable for the life of the process through /proc/<pid>/cmdline,
//     and a PR body can legitimately contain anything.
//  3. Output is requested as JSON (`--json`) wherever gh supports it, so the
//     CLI renders structured data rather than reformatting human text.

const (
	// ghProxyTimeout bounds a gh call. gh is an API client, not a transport,
	// so it should be quicker than git's network operations; a slow one is
	// GitHub being slow or rate-limiting.
	ghProxyTimeout = 60 * time.Second

	// ghProxyLogTimeout bounds a CI-log read. `gh run view --log-failed` does
	// not call one endpoint: it downloads the run's whole log archive (and,
	// when GitHub cannot associate jobs with it, falls back to fetching each
	// job's log individually). A large matrix build legitimately needs longer
	// than an API call, so it gets its own bound rather than making every
	// other verb wait three minutes for a hung one.
	ghProxyLogTimeout = 180 * time.Second

	// ghProxyCommentsTimeout is the TOTAL budget for `pr comments`, which is
	// two gh calls (the conversation, then the inline review threads). A
	// budget rather than two independent bounds, so the daemon's worst case
	// stays a number the CLI can wait on rather than the sum of whatever the
	// verb happens to do next.
	ghProxyCommentsTimeout = 90 * time.Second

	// ghProxyDownloadTimeout is the TOTAL budget for `run download`: the
	// artifact manifest read, then gh's own download and unzip. It is the
	// longest bound in this file because it is the only verb that moves bulk
	// bytes onto disk rather than into a response — maxGHArtifactBytes of
	// them, over a link the daemon does not control.
	ghProxyDownloadTimeout = 300 * time.Second

	// maxGHProxyTextBytes is the tail kept from a verb whose output IS the
	// payload — a comment thread, a failed job's log — rather than a
	// diagnosis. The default 16 KiB is right for "what went wrong with this
	// push"; it would cut a CodeRabbit review or a Go test failure off in the
	// middle. The tail is still what is kept, and it is still the useful end:
	// comments render oldest-first, and a failing step's error is at the end
	// of its log.
	maxGHProxyTextBytes = 256 * 1024

	// maxGHProxyBodyBytes bounds a PR/issue body or comment. GitHub's own
	// limit is 65536 characters; this is that, with headroom for multi-byte
	// runes, and it is enforced before anything is written to disk.
	maxGHProxyBodyBytes = 256 * 1024

	// maxGHProxyTitleLen bounds a PR title. GitHub truncates around 256; a
	// title longer than this is a body in the wrong field.
	maxGHProxyTitleLen = 256

	// maxGHProxyLimit bounds a list request.
	maxGHProxyLimit     = 100
	defaultGHProxyLimit = 20

	// ghArtifactDirName is the ONE directory a download may land in, relative
	// to the agent's own work-tree root. It is a constant rather than a
	// parameter on purpose: every other verb in this proxy lends the operator's
	// credential without lending filesystem reach, and a caller-supplied
	// destination is exactly the parameter that would break that (see
	// artifactDest for how the path is realized).
	//
	// The leading dot and the self-ignoring .gitignore artifactDest writes keep
	// a download out of `git status`, so an agent that downloads an artifact
	// mid-branch does not then commit it by reflex.
	ghArtifactDirName = ".tclaude-artifacts"

	// maxGHArtifactBytes caps a single download. Artifacts are routinely
	// gigabytes — a CI job that uploads a build tree does not think of itself
	// as unusual — and this is the one verb where an agent's mistake costs the
	// operator's disk rather than their context window. The manifest read that
	// precedes the download is what makes the cap enforceable: gh has no size
	// limit of its own to ask for.
	//
	// It caps the ZIP size, which is the only figure GitHub reports. gh unzips,
	// so it does NOT bound the footprint on disk — see
	// maxGHArtifactUnpackedBytes, which does.
	maxGHArtifactBytes = 512 << 20

	// maxGHArtifactRuns caps how many run directories are KEPT. Without it the
	// per-download bounds mean nothing in aggregate: each run id gets its own
	// directory, nothing ever removed one, and a caller with an endless supply
	// of run ids could fill a disk one bounded download at a time.
	//
	// Three rather than one so that comparing a red run against a green one
	// still works, which is a real thing to want. The least recently touched
	// are pruned first; with maxGHArtifactUnpackedBytes this bounds the whole
	// store at three times that figure.
	maxGHArtifactRuns = 3

	// maxGHArtifactEntries bounds the file listing reported back. A build-tree
	// artifact has tens of thousands of files, and enumerating them would push
	// the useful part — where the download landed — out of a bounded response.
	maxGHArtifactEntries = 200

	// maxGHArtifactListingBytes bounds that same listing in BYTES, because a
	// count alone does not bound it: the paths inside an artifact are chosen by
	// whoever configured the CI job, and 200 deeply-nested ones run to several
	// hundred kilobytes. The listing is a response the idempotency middleware
	// may persist for its full TTL, which is the cost that bound exists for.
	maxGHArtifactListingBytes = 64 * 1024

	// maxGHArtifactNamesReported bounds the artifact names echoed back in a
	// "no such artifact" refusal. Enough to choose from, far short of a page of
	// 255-character names.
	maxGHArtifactNamesReported = 25

	// maxGHArtifactNameLen bounds an artifact name. GitHub's own limit is
	// smaller; this leaves room and still refuses a body in the wrong field.
	maxGHArtifactNameLen = 200
)

// maxGHArtifactUnpackedBytes caps what ONE download may leave on disk, and it
// exists because maxGHArtifactBytes cannot do that job: GitHub reports the
// compressed size, and deflate on repetitive content runs to ratios in the
// hundreds. An artifact well under the zip cap can therefore unpack to far more
// than any disk holds — and on a public repository a fork's pull request can
// upload one, which `run download` will happily fetch.
//
// It is enforced after extraction, from the walk the listing does anyway: gh
// offers no way to bound the unpack itself, so the transient peak during
// extraction is not covered. What is covered is that nothing oversized is left
// behind, which is what a repeated-download disk attack needs.
//
// A var rather than a const only so a test can prove the refusal without
// materializing two gigabytes on the runner's disk.
var maxGHArtifactUnpackedBytes int64 = 2 << 30

// SetMaxArtifactUnpackedBytesForTest lowers the on-disk cap so the refusal path
// can be exercised against a few megabytes. Returns a restore func.
func SetMaxArtifactUnpackedBytesForTest(n int64) func() {
	prev := maxGHArtifactUnpackedBytes
	maxGHArtifactUnpackedBytes = n
	return func() { maxGHArtifactUnpackedBytes = prev }
}

// ghProxySession is a gh invocation context: the repo slug the agent's own
// remote resolved to, plus the resolved credentials.
type ghProxySession struct {
	ghPath    string
	ownerRepo string
	remoteKey string
	env       []string
	neutral   string
	// branch is the agent's current branch, resolved daemon-side while the git
	// session is still open. `pr create` needs it: gh derives the head branch
	// from the local repository, and this proxy deliberately runs gh in a
	// neutral directory where there is none.
	branch string
	// repoRoot is the agent's own work-tree root, already symlink-resolved and
	// gated by resolveProxyRepo. Only `run download` uses it, and only as the
	// root it is not allowed to write outside of — gh still RUNS in neutral.
	repoRoot string
}

// newGHProxySession runs every gate and resolves the GitHub repository from
// the agent's own remote.
//
// Note the ordering: the git-side gates run FIRST and in full. A caller with
// proxy.github.read still cannot reach a repository whose remote is not on the
// operator's allow-list, because the allow-list check happens before the repo
// slug is even derived.
func newGHProxySession(ctx context.Context, convID, requestedRemote string, remoteScoped bool) (*ghProxySession, *proxyFault) {
	s, resolved, fault := openProxyRemote(ctx, convID, requestedRemote, remoteScoped)
	if fault != nil {
		return nil, fault
	}
	if resolved.FetchRef.Host != "github.com" {
		return nil, faultf(http.StatusConflict, "not_github",
			"remote %q points at %s, which is not GitHub; the github proxy only speaks to github.com",
			resolved.Name, resolved.FetchRef.Host)
	}
	// EXACTLY two path segments. A GitHub repository is always owner/repo, and
	// accepting more re-derives the slug from a path the allow-list matched
	// under different rules — which is an allow-list escape, not a nicety:
	//
	//   allow-list  github.com/acme/widgets        (the "one repo" form)
	//   remote      github.com/acme/widgets/secret
	//
	// matchRemotePattern admits the remote, because a pattern shorter than the
	// target matches as a PREFIX (deliberate, for nested GitLab groups). But
	// OwnerRepo() is first+last, so the slug becomes acme/secret — a repository
	// the operator never allow-listed, reachable with their credential. The git
	// half is unaffected (GitHub 404s a four-segment path); it is only here,
	// where the slug is re-derived, that the two rules disagree.
	if len(resolved.FetchRef.Path) != 2 {
		return nil, faultf(http.StatusConflict, "not_github",
			"remote %q resolves to %s, which is not a plain github owner/repo path; "+
				"the github proxy will not derive a repository from it",
			resolved.Name, resolved.FetchRef.Key())
	}
	ownerRepo := resolved.FetchRef.OwnerRepo()
	owner, repo, _ := strings.Cut(ownerRepo, "/")
	if !isGitHubOwnerSlug(owner) || !isGitHubRepoSlug(repo) {
		return nil, faultf(http.StatusConflict, "not_github",
			"remote %q does not resolve to a valid github owner/repo pair", resolved.Name)
	}
	ghPath, err := proxyBinary("gh")
	if err != nil {
		return nil, faultf(http.StatusServiceUnavailable, "tool_missing", "%v", err)
	}
	env, fault := ghProxyEnv(s.policy)
	if fault != nil {
		return nil, fault
	}
	return &ghProxySession{
		ghPath:    ghPath,
		ownerRepo: ownerRepo,
		remoteKey: resolved.FetchRef.Key(),
		env:       env,
		// Read while the git session is still open — the gh half has no
		// repository of its own to ask.
		branch:   s.currentBranch(ctx),
		repoRoot: s.repoRoot,
		// A neutral working directory is a security control, not tidiness:
		// running gh inside the agent's repository would let .git/config
		// re-aim it despite the explicit --repo.
		neutral: os.TempDir(),
	}, nil
}

// ghProxyEnv builds gh's environment from scratch, for the same reason
// gitProxyEnv does: an allow-list cannot drift, a deny-list can.
//
// GH_TOKEN is set only when the operator configured a token file. Otherwise gh
// authenticates from its own configuration under the daemon's HOME, which is
// the ordinary posture and keeps the secret out of the child's environment.
func ghProxyEnv(policy config.GitProxyConfig) ([]string, *proxyFault) {
	env := []string{
		"LC_ALL=C",
		// gh opens a browser and prompts when it thinks it is interactive.
		// The daemon has no one to prompt.
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"NO_COLOR=1",
	}
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "XDG_CONFIG_HOME"} {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			env = append(env, name+"="+v)
		}
	}
	configured := strings.TrimSpace(policy.GitHubTokenFile)
	if configured == "" {
		return env, nil
	}
	// "~/github-token.txt" is how an operator naturally writes this in a JSON
	// config file, and the same expandTilde every other human-typed path in the
	// daemon goes through applies here.
	tokenFile := expandTilde(configured)
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, faultf(http.StatusServiceUnavailable, "token_unreadable",
			"the configured agent.git_proxy.github_token_file could not be read: %v%s",
			err, shellVarHint(configured))
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, faultf(http.StatusServiceUnavailable, "token_unreadable",
			"the configured agent.git_proxy.github_token_file is empty")
	}
	// Environment, never argv: /proc/<pid>/cmdline is readable by any
	// same-uid process for the life of the child.
	return append(env, "GH_TOKEN="+token), nil
}

// gh runs a gh subcommand. args must already be fully validated — nothing in
// this function inspects them, which is why every caller builds its argv from
// fixed literals plus values that have passed a validateGH* gate.
//
// Each caller supplies its own `--repo g.ownerRepo`. It is a per-subcommand
// flag in gh, so it cannot be prepended here; the neutral working directory is
// what makes forgetting it fail loudly (gh reports "none of the git remotes
// configured for this repository point to a known GitHub host") rather than
// silently acting on whatever repository happened to be in scope.
func (g *ghProxySession) gh(ctx context.Context, args ...string) (ProxyResult, error) {
	return g.ghBounded(ctx, ghProxyTimeout, 0, args...)
}

// ghBulk runs a gh subcommand whose output is the payload rather than a
// diagnosis, so it keeps a much larger tail. Same argv contract as gh: every
// value has already passed a validateGH* gate.
func (g *ghProxySession) ghBulk(ctx context.Context, timeout time.Duration, args ...string) (ProxyResult, error) {
	return g.ghBounded(ctx, timeout, maxGHProxyTextBytes, args...)
}

// ghBounded is the shared body. maxOutput of 0 takes the daemon-wide default.
func (g *ghProxySession) ghBounded(ctx context.Context, timeout time.Duration, maxOutput int, args ...string) (ProxyResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return proxyExec(runCtx, ProxyCommand{
		Tool:           "gh",
		Path:           g.ghPath,
		Args:           append([]string(nil), args...),
		Dir:            g.neutral,
		Env:            g.env,
		MaxOutputBytes: maxOutput,
	})
}

// bodyFile writes free text to a 0600 file under TMPDIR and returns its path
// plus a cleanup func. The file is how a PR body reaches gh without ever
// appearing in argv, where /proc would expose it for the life of the process.
//
// The mode, not the location, is what protects it: this is an ordinary temp
// file, removed as soon as gh has run.
func (g *ghProxySession) bodyFile(body string) (string, func(), *proxyFault) {
	f, err := os.CreateTemp("", "tclaude-ghproxy-*.md")
	if err != nil {
		return "", func() {}, faultf(http.StatusInternalServerError, "io",
			"could not stage the message body: %v", err)
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	if err := f.Chmod(0o600); err != nil {
		cleanup()
		return "", func() {}, faultf(http.StatusInternalServerError, "io",
			"could not secure the staged message body: %v", err)
	}
	if _, err := f.WriteString(body); err != nil {
		cleanup()
		return "", func() {}, faultf(http.StatusInternalServerError, "io",
			"could not write the staged message body: %v", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, faultf(http.StatusInternalServerError, "io",
			"could not finish the staged message body: %v", err)
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

// ---------------------------------------------------------------------------
// Artifact destination
// ---------------------------------------------------------------------------

// artifactDest prepares, and returns the absolute path of, the directory a
// download for runID lands in: <work-tree root>/.tclaude-artifacts/run-<id>/.
//
// The destination is COMPUTED, never supplied. That is the same rule as the
// repository slug, for the same reason: agentd runs unsandboxed and a caller
// who can name the directory can have the daemon write wherever the daemon can
// reach, which is the one thing "lends credentials, not filesystem reach" is
// meant to rule out. There is no path parameter to validate because there is
// no path parameter.
//
// Every step goes through os.Root rather than plain os calls. A Root refuses
// any traversal that leaves the work tree, so a `.tclaude-artifacts` the agent
// has replaced with a symlink to ~/.ssh fails here instead of being followed —
// which plain MkdirAll would do, and which os.Lstat could only detect in a
// window the agent gets to race.
//
// What remains is gh's own write: it receives an ordinary path string and
// resolves it itself, so an agent that swaps a component AFTER this returns can
// still redirect the extraction. That race is real, bounded by the same-uid
// reality the whole proxy sits inside (docs/git-proxy.md), and deliberately not
// papered over here.
func (g *ghProxySession) artifactDest(runID string) (string, *proxyFault) {
	if g.repoRoot == "" {
		return "", faultf(http.StatusConflict, "repo_unresolved",
			"the daemon could not resolve your work tree, so there is nowhere to put a download")
	}
	root, err := os.OpenRoot(g.repoRoot)
	if err != nil {
		return "", faultf(http.StatusConflict, "repo_unresolved",
			"your work tree %s is not reachable: %v", g.repoRoot, err)
	}
	defer func() { _ = root.Close() }()

	if err := root.MkdirAll(ghArtifactDirName, 0o755); err != nil {
		return "", faultf(http.StatusConflict, "artifact_dir",
			"could not prepare %s in your work tree: %v", ghArtifactDirName, err)
	}
	// A directory whose contents are all ignored does not show up as untracked
	// at all, so this keeps downloads out of `git status` without the operator
	// having to edit .gitignore. Written every time: the agent may have deleted
	// it, and rewriting three bytes is cheaper than deciding whether to.
	ignore := path.Join(ghArtifactDirName, ".gitignore")
	if err := root.WriteFile(ignore, []byte("*\n"), 0o644); err != nil {
		return "", faultf(http.StatusConflict, "artifact_dir",
			"could not write %s: %v", ignore, err)
	}

	rel := path.Join(ghArtifactDirName, "run-"+runID)
	// Fresh every time. Left in place, an earlier download's files would be
	// listed back as part of this one — and a caller reading that listing has
	// no way to tell which run a file came from.
	//
	// This is also why downloading the SAME run repeatedly cannot accumulate:
	// each attempt starts by removing what the last one left.
	if err := root.RemoveAll(rel); err != nil {
		return "", faultf(http.StatusConflict, "artifact_dir",
			"could not clear a previous download at %s: %v", rel, err)
	}
	// DIFFERENT runs are the case that does accumulate, and the one that has
	// to be pruned: every run id gets its own directory, so without this a
	// caller could fill a disk one individually-bounded download at a time.
	if fault := pruneArtifactRuns(root, runID); fault != nil {
		return "", fault
	}
	if err := root.Mkdir(rel, 0o755); err != nil {
		return "", faultf(http.StatusConflict, "artifact_dir",
			"could not create %s: %v", rel, err)
	}
	return filepath.Join(g.repoRoot, filepath.FromSlash(rel)), nil
}

// pruneArtifactRuns removes the least recently touched run directories until
// at most maxGHArtifactRuns-1 remain besides the one about to be created.
//
// Everything goes through the same os.Root as the rest of artifactDest, so a
// pruned entry can only ever be inside the work tree, and only ever one whose
// name this proxy generates: `run-<digits>`, matched strictly. A directory the
// agent dropped in beside them under any other name is not this proxy's to
// delete, and is left alone.
//
// The two failure modes are deliberately NOT treated alike:
//
//   - A single directory that will not delete is tolerated. Refusing a
//     legitimate download over one stale directory trades a real capability for
//     a housekeeping problem, and it does not get the caller anywhere.
//   - A directory that cannot be ENUMERATED refuses the download, because
//     without the listing there is no bound, and "prune nothing, carry on" is
//     the one outcome that would quietly restore unbounded accumulation.
//
// The agent owns this directory, so the question is whether it can arrange the
// second case as an opt-out of the cap. Through the obvious route it cannot:
// os.Root traverses by opening each component, which needs the read bit, so a
// `chmod` that would blind ReadDir also fails the MkdirAll and WriteFile above
// and the download is refused before reaching here. What is left is the
// directory changing under us mid-request, where refusing is still the right
// answer and costs the caller only its own download.
func pruneArtifactRuns(root *os.Root, keepRunID string) *proxyFault {
	entries, err := fs.ReadDir(root.FS(), ghArtifactDirName)
	if err != nil {
		return faultf(http.StatusConflict, "artifact_dir",
			"could not list %s, so the limit on kept downloads cannot be applied and this "+
				"download is refused rather than allowed to accumulate: %v",
			ghArtifactDirName, err)
	}
	type runDir struct {
		name string
		mod  time.Time
	}
	var runs []runDir
	keep := "run-" + keepRunID
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keep || !isArtifactRunDir(e.Name()) {
			continue
		}
		mod := time.Time{}
		if info, statErr := e.Info(); statErr == nil {
			mod = info.ModTime()
		}
		runs = append(runs, runDir{name: e.Name(), mod: mod})
	}
	if len(runs) < maxGHArtifactRuns {
		return nil
	}
	// Oldest first, so the ones that go are the ones least recently wanted.
	slices.SortFunc(runs, func(a, b runDir) int { return a.mod.Compare(b.mod) })
	for _, r := range runs[:len(runs)-(maxGHArtifactRuns-1)] {
		_ = root.RemoveAll(path.Join(ghArtifactDirName, r.name))
	}
	return nil
}

// isArtifactRunDir matches only the names artifactDest generates. Anything
// else in the artifacts directory belongs to whoever put it there.
func isArtifactRunDir(name string) bool {
	digits, ok := strings.CutPrefix(name, "run-")
	if !ok || digits == "" {
		return false
	}
	return strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// artifactListing walks a completed download and renders what landed: the
// destination, then one line per file, then a total.
//
// It is the daemon's own text rather than gh's — `gh run download` prints
// nothing at all on success, which for a verb whose whole effect is on disk
// tells the caller neither where to look nor whether anything arrived.
//
// filepath.WalkDir lstats, so a symlink inside an extracted artifact is
// reported as an entry and never followed.
//
// It takes the request's ctx because the two listing bounds cap what is
// REPORTED, not what is walked: `files` and `total` describe the whole tree, so
// the walk visits every entry. An artifact well inside the 512 MiB zip cap can
// unpack to millions of small files, and this runs on the request goroutine
// after gh has returned — so the walk is held under the download's own
// deadline rather than being allowed to run past it unbounded.
//
// Not "so a disconnected client stops it": this route is a POST outside
// bulkReadRoutes, so the idempotency middleware buffers and PERSISTS the
// response either way, and a reconnecting client replays whatever this
// produced for the full TTL. That is exactly why a stopped walk has to be
// labelled honestly below — the degraded listing becomes the durable answer.
// It returns the walk as well as the text, because the byte total is what
// maxGHArtifactUnpackedBytes is enforced against and the walk is where it is
// already known.
func artifactListing(ctx context.Context, dest string) (string, artifactWalk) {
	type entry struct {
		rel  string
		size string
	}
	var (
		entries   []entry
		files     int
		total     int64
		unsized   int
		listBytes int
		walkErr   error
	)
	var stopErr error
	walkErr = filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Which error it is decides what the caller should DO, so keep it
		// rather than reducing it to a bool.
		if e := ctx.Err(); e != nil {
			stopErr = e
			return fs.SkipAll
		}
		if d.IsDir() {
			return nil
		}
		files++
		size := "?"
		// A file whose size cannot be read is reported as such rather than as
		// zero, and counted, so the footer total cannot silently understate
		// what is on disk.
		if info, statErr := d.Info(); statErr == nil {
			total += info.Size()
			size = humanBytes(info.Size())
		} else {
			unsized++
		}
		// TWO bounds, because the paths inside an artifact are chosen by
		// whoever configured the CI job — on a public repository, by anyone who
		// can open a pull request. maxGHArtifactEntries alone bounds the count
		// but not the length, and this listing is a response the daemon may
		// persist for the idempotency TTL.
		if len(entries) < maxGHArtifactEntries && listBytes < maxGHArtifactListingBytes {
			rel, relErr := filepath.Rel(dest, p)
			if relErr != nil {
				rel = p
			}
			rel = filepath.ToSlash(rel)
			listBytes += len(rel) + len(size) + 2
			entries = append(entries, entry{rel: rel, size: size})
		}
		return nil
	})

	stopped := stopErr != nil
	// A stopped walk's total is a floor, so it must NOT be presented as a
	// figure the size cap can be judged against — see artifactWalk.Complete.
	walk := artifactWalk{Bytes: total, Files: files, Complete: !stopped && walkErr == nil}

	var b strings.Builder
	fmt.Fprintf(&b, "downloaded to %s\n\n", dest)
	// A walk error does NOT discard the entries already collected. One
	// unreadable file is not a reason to answer "here is nothing" about a
	// download that succeeded.
	for _, e := range entries {
		fmt.Fprintf(&b, "%s\t%s\n", e.rel, e.size)
	}
	if files > len(entries) {
		// "at least" here as well as in the footer: a stopped walk's `files` is
		// itself only what was reached, so a bare remainder would be a definite
		// number the code knows is a floor — and could understate by orders of
		// magnitude on the very tree that stopped it.
		fmt.Fprintf(&b, "… and %s%d more\n", atLeast(stopped), files-len(entries))
	}
	if files == 0 && walkErr == nil && !stopped {
		// The preflight has already ruled out "no such artifact" and "expired",
		// so this is gh having unpacked an artifact that genuinely holds no
		// files. Saying anything more would be guessing.
		b.WriteString("(the artifact unpacked to no files at all)\n")
		return b.String(), walk
	}
	fmt.Fprintf(&b, "\n(%s%s in %s", atLeast(stopped), pluralFiles(files), humanBytes(total))
	if unsized > 0 {
		fmt.Fprintf(&b, ", %d of unreadable size and not counted", unsized)
	}
	b.WriteString(")\n")
	if stopped {
		// The two causes want OPPOSITE advice, and the daemon's 300s bound is
		// SHORTER than the CLI's 330s — so the deadline case is one the caller
		// is still connected to read. Telling it "cancelled" would be false,
		// and the natural response to a truncated listing is to download again,
		// which clears the destination first: the retry would delete the tree
		// that just landed and re-download it against the same budget.
		if errors.Is(stopErr, context.DeadlineExceeded) {
			b.WriteString("(the listing ran out of time — but the DOWNLOAD finished, so the files " +
				"really are at the path above. List them yourself rather than downloading " +
				"again: a second download clears the directory first and would spend the same " +
				"budget to arrive back here.)\n")
		} else {
			b.WriteString("(the listing stopped early — the request went away; " +
				"the files are on disk regardless)\n")
		}
	}
	if walkErr != nil {
		fmt.Fprintf(&b, "(the listing is incomplete — the directory could not be fully read: %v)\n", walkErr)
	}
	return b.String(), walk
}

// artifactWalk is what the listing walk learned about the tree on disk, as
// opposed to what it printed.
//
// Complete is the field that matters: a walk stopped by the deadline, or cut
// short by an unreadable directory, has a Bytes that is a FLOOR. Enforcing a
// size cap against a floor would refuse downloads at random and, worse, pass
// oversized ones whose walk happened to stop early — so the cap is only
// applied when this is true.
type artifactWalk struct {
	Bytes    int64
	Files    int
	Complete bool
}

// atLeast marks a count the walk did not finish gathering, so "3 files" cannot
// be read as the whole of what is on disk when it is only what was counted
// before the request went away.
func atLeast(stopped bool) string {
	if stopped {
		return "at least "
	}
	return ""
}

func pluralFiles(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

// humanBytes renders a size the way a human reads one. Sizes appear in a
// listing an agent reads as prose, where "1.2 MiB" carries the judgement
// "1258291" does not.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// ---------------------------------------------------------------------------
// Parameter validation
// ---------------------------------------------------------------------------

// validateGHArtifactName bounds an artifact name. It reaches argv as `gh run
// download -n <name>`, so it is charset-checked like a title rather than passed
// through like a body: a leading "-" would be read as a flag, and a path
// separator would be an attempt to steer where gh writes.
//
// GitHub itself refuses `" : < > | * ? \ /` and control characters in an
// artifact name, so nothing legal is lost by refusing them here too.
func validateGHArtifactName(name string) (string, *proxyFault) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if utf8.RuneCountInString(name) > maxGHArtifactNameLen {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"the artifact name is longer than %d characters", maxGHArtifactNameLen)
	}
	if strings.HasPrefix(name, "-") {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"an artifact name may not begin with '-'")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", faultf(http.StatusBadRequest, "invalid_arg",
				"the artifact name contains a control character")
		}
		if strings.ContainsRune(`/\:<>|*?"`, r) {
			return "", faultf(http.StatusBadRequest, "invalid_arg",
				"the artifact name contains %q, which GitHub does not allow in one", r)
		}
	}
	return name, nil
}

// validateGHNumber bounds a PR/issue number. It is rendered back into argv as
// a decimal string, so parsing it into an int and re-formatting it is what
// guarantees no other character can survive.
func validateGHNumber(n int) (string, *proxyFault) {
	if n <= 0 || n > 100_000_000 {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"a positive pull-request/issue number is required")
	}
	return strconv.Itoa(n), nil
}

// validateGHRunID bounds a GitHub Actions workflow-run id. It gets its own
// validator rather than reusing validateGHNumber because the two live in
// different number spaces: PR numbers are per-repository and small, while run
// ids are global database ids already past 10^10 — validateGHNumber's ceiling
// would refuse every real one.
//
// The upper bound is 2^53, the largest integer a JSON number carries exactly.
// Anything above it did not survive the wire intact, so refusing it is honest
// rather than restrictive. Like validateGHNumber, the value that reaches argv
// is re-formatted from the parsed integer, never the caller's string.
func validateGHRunID(id int64) (string, *proxyFault) {
	if id <= 0 || id > 1<<53 {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"a positive workflow-run id is required")
	}
	return strconv.FormatInt(id, 10), nil
}

// validateGHBody bounds free text. Unlike every other parameter here the body
// is deliberately unrestricted in charset — it is prose that will be published
// — which is exactly why it travels by file rather than by argv.
func validateGHBody(body string, required bool) *proxyFault {
	if strings.TrimSpace(body) == "" {
		if required {
			return faultf(http.StatusBadRequest, "invalid_arg", "a body is required")
		}
		return nil
	}
	if len(body) > maxGHProxyBodyBytes {
		return faultf(http.StatusBadRequest, "invalid_arg",
			"body is %d bytes; the maximum is %d", len(body), maxGHProxyBodyBytes)
	}
	return nil
}

// validateGHTitle bounds a PR title. A title DOES reach argv (gh has no
// --title-file), so it is charset-checked: control characters are refused and
// a leading "-" would be read as a flag.
func validateGHTitle(title string) *proxyFault {
	title = strings.TrimSpace(title)
	if title == "" {
		return faultf(http.StatusBadRequest, "invalid_arg", "a title is required")
	}
	// Runes, not bytes: maxGHProxyTitleLen and GitHub's own limit are both
	// stated in characters, so a byte count would refuse a perfectly legal
	// non-ASCII title at well under a third of the real limit.
	if utf8.RuneCountInString(title) > maxGHProxyTitleLen {
		return faultf(http.StatusBadRequest, "invalid_arg",
			"title is longer than %d characters", maxGHProxyTitleLen)
	}
	if strings.HasPrefix(title, "-") {
		return faultf(http.StatusBadRequest, "invalid_arg",
			"a title may not begin with '-'")
	}
	for _, r := range title {
		if r < 0x20 || r == 0x7f {
			return faultf(http.StatusBadRequest, "invalid_arg",
				"the title contains a control character (did you mean to put this in the body?)")
		}
		// Unicode format characters — the bidirectional overrides U+202E and
		// U+2066..U+2069 above all — reorder how the title RENDERS without
		// changing what it contains. This title is published under the
		// operator's own account, where a reader has no reason to suspect the
		// displayed text is not the stored text.
		if unicode.Is(unicode.Cf, r) {
			return faultf(http.StatusBadRequest, "invalid_arg",
				"the title contains a Unicode format character (U+%04X); those can reorder how the "+
					"title renders without changing what it says", r)
		}
	}
	return nil
}

// validateGHState bounds a list filter to gh's own vocabulary. An allow-list
// of literals, so the value that reaches argv is one of these constants and
// never the caller's string.
func validateGHState(state string, allowed ...string) (string, *proxyFault) {
	state = strings.ToLower(strings.TrimSpace(state))
	if state == "" {
		return allowed[0], nil
	}
	for _, a := range allowed {
		if state == a {
			return a, nil
		}
	}
	return "", faultf(http.StatusBadRequest, "invalid_arg",
		"state %q is not one of: %s", state, strings.Join(allowed, ", "))
}

// ghRunStatuses is gh's own `run list --status` vocabulary, verbatim (gh 2.97).
// An allow-list of literals, so the value that reaches argv is one of these
// constants and never the caller's string.
//
// This is the AUTHORITY. The CLI keeps its own copy for shell completion
// because it must not import the daemon; TestGHRunStatusCompletionMatchesTheGate
// pins the two together, so a status added here cannot silently stop being
// offered there.
var ghRunStatuses = []string{
	"queued", "completed", "in_progress", "requested", "waiting", "pending",
	"action_required", "cancelled", "failure", "neutral", "skipped", "stale",
	"startup_failure", "success", "timed_out",
}

// validateGHRunStatus bounds the run-list filter. It differs from
// validateGHState in what an EMPTY value means: there, empty picks the first
// allowed state as a default; here it means no filter at all, because "every
// recent run" is the sensible default listing and there is no one status that
// could stand in for it.
func validateGHRunStatus(status string) (string, *proxyFault) {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		return "", nil
	}
	for _, a := range ghRunStatuses {
		if status == a {
			return a, nil
		}
	}
	return "", faultf(http.StatusBadRequest, "invalid_arg",
		"status %q is not one of: %s", status, strings.Join(ghRunStatuses, ", "))
}

// GHRunStatusesForTest exposes the gate's vocabulary so the CLI's completion
// copy can be pinned against it.
func GHRunStatusesForTest() []string { return append([]string(nil), ghRunStatuses...) }

func validateGHLimit(limit int) (string, *proxyFault) {
	if limit == 0 {
		limit = defaultGHProxyLimit
	}
	if limit < 1 || limit > maxGHProxyLimit {
		return "", faultf(http.StatusBadRequest, "invalid_arg",
			"limit must be between 1 and %d", maxGHProxyLimit)
	}
	return strconv.Itoa(limit), nil
}

// ---------------------------------------------------------------------------
// Response shape
// ---------------------------------------------------------------------------

// ghProxyOutcome mirrors gitProxyOutcome: HTTP 200 means the daemon ran gh,
// not that gh succeeded. ExitCode carries gh's verdict.
//
// Stdout is passed through as a raw JSON message when gh produced JSON, so the
// CLI can render it without the daemon having to model every gh schema. That
// is deliberate: modelling them here would mean a daemon release every time
// GitHub adds a field.
type ghProxyOutcome struct {
	Repo      string          `json:"repo"`
	ExitCode  int             `json:"exit_code"`
	JSON      json.RawMessage `json:"json,omitempty"`
	Stdout    string          `json:"stdout,omitempty"`
	Stderr    string          `json:"stderr,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
	TimedOut  bool            `json:"timed_out,omitempty"`
}

// respond renders a gh result. When gh emitted JSON it rides in the JSON
// field; otherwise the raw text does. gh's own error text always reaches the
// agent verbatim, because "GraphQL: Resource not accessible by integration" is
// the actionable part of a failure.
func (g *ghProxySession) respond(w http.ResponseWriter, r *http.Request, verb string, res ProxyResult, err error) {
	if err != nil {
		writeError(w, http.StatusBadGateway, "gh_failed", err.Error())
		return
	}
	out := ghProxyOutcome{
		Repo:      g.ownerRepo,
		ExitCode:  res.ExitCode,
		Stderr:    res.Stderr,
		Truncated: res.Truncated,
		TimedOut:  res.TimedOut,
	}
	trimmed := strings.TrimSpace(res.Stdout)
	if res.ExitCode == 0 && !res.Truncated && json.Valid([]byte(trimmed)) && trimmed != "" {
		out.JSON = json.RawMessage(trimmed)
	} else {
		out.Stdout = res.Stdout
	}
	setAuditDetail(r, fmt.Sprintf("repo=%s op=%s exit=%d", g.ownerRepo, verb, res.ExitCode))
	writeJSON(w, http.StatusOK, out)
}
