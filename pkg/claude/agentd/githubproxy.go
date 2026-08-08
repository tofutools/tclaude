package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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
)

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
}

// newGHProxySession runs every gate and resolves the GitHub repository from
// the agent's own remote.
//
// Note the ordering: the git-side gates run FIRST and in full. A caller with
// github.read still cannot reach a repository whose remote is not on the
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
		branch: s.currentBranch(ctx),
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
// Parameter validation
// ---------------------------------------------------------------------------

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
