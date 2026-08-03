package agentd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/resumeprovenance"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// gitproxy.go is the daemon half of `tclaude agent git` — a *semantic* proxy
// that lets a sandboxed agent reach a Git remote without ever holding the
// credential.
//
// The shape of the deal
//
// A sandbox profile can deny ~/.ssh and ~/.config/gh (both ship in the
// dashboard's common-rule catalog) and can deny or filter the network. That is
// the posture we want, but it also stops the agent doing the one thing it was
// spawned for. agentd runs unsandboxed on the host and already holds those
// credentials, so it performs the network half on the agent's behalf.
//
// The invariant that keeps this honest: THE PROXY LENDS CREDENTIALS, NEVER
// FILESYSTEM REACH. Every operation here is one the agent could already have
// performed on files it can already write; the only thing it lacked was the
// secret. resolveProxyRepo is what enforces that — see its doc comment.
//
// Why the repo's own configuration is treated as hostile
//
// `.git/config`, `.git/hooks/*` and `.gitattributes` all live in a directory
// the agent can write. Every one of them can name a program for git to run.
// So this file never trusts repo-local configuration for anything
// security-relevant:
//
//   - Hooks are disabled outright (core.hooksPath points at a daemon-owned
//     empty directory). This covers pre-push, post-merge and
//     reference-transaction, the last of which fires on ref updates during
//     BOTH fetch and push.
//   - Only network-half operations run here — fetch, push, ls-remote. None of
//     them updates the working tree, so .gitattributes smudge/clean filters
//     never execute. `tclaude agent git pull` is deliberately split: the
//     daemon fetches, and the fast-forward runs in the agent's own process
//     where it needs no credential. See the CLI side.
//   - The remote URL is validated rather than trusted. `ext::` URLs execute a
//     command outright and `file:` escapes the allow-list, so the transport
//     is pinned to https/ssh at the git level AND the parsed URL is
//     independently required to be one of those two.
//   - remote.<name>.uploadpack / receivepack / vcs / proxy select a PROGRAM
//     rather than a destination. A `-c` override does NOT displace the first
//     two — git reads them first-wins across scopes, so a repo-local value
//     beats the command line — so the proxy does two things instead: it
//     refuses outright when a repository sets any of them
//     (refuseDangerousRemoteConfig), and it passes the stock programs as
//     --upload-pack / --receive-pack FLAGS, which do override config.
//   - core.sshCommand, core.alternateRefsCommand, core.fsmonitor,
//     core.editor, core.pager, gpg.program, diff.external and http.proxy are
//     all pinned, because each is a command-execution or redirection vector.
//   - url.<base>.insteadOf cannot be reset by a `-c` override, so it is
//     defeated by a fixed-point check instead: a repo whose configuration
//     would rewrite the URL we validated is refused rather than followed.
//     See resolveProxyRemote.
//   - credential.helper is reset and then re-populated from GLOBAL/SYSTEM
//     configuration only, so an operator's real helper keeps working while a
//     repo-local one — an arbitrary command — is dropped.
//
// Nothing here ever runs a shell, and no agent-supplied string ever becomes a
// git flag: every parameter is charset-validated and refused if it begins with
// "-".
//
// What this is NOT
//
// agentd's permission layer is a coordination guardrail, not a security
// boundary (docs/sandbox-hardening.md). This feature does not change that. A
// same-uid agent that is not actually confined by the OS sandbox can read
// ~/.ssh directly and has no need of the proxy. The proxy is what makes
// *denying* those paths survivable; it is not what enforces the denial.

const (
	// gitProxyNetworkTimeout bounds a credentialed network operation. Generous
	// — a first push of a large branch over a slow link is legitimately slow —
	// but bounded, so a hung transport can never pin a daemon goroutine.
	gitProxyNetworkTimeout = 120 * time.Second

	// gitProxyProbeTimeout bounds the purely local probes (rev-parse, remote
	// get-url, config reads). These touch no network, so a slow one means
	// something is wrong rather than merely far away.
	gitProxyProbeTimeout = 15 * time.Second

	// gitProxyMaxOutputBytes is the per-stream rolling tail kept from a proxied
	// subprocess. Git's progress output is unbounded; the agent needs the end
	// of it (the error, the ref summary), not the middle.
	gitProxyMaxOutputBytes = 16 * 1024

	// maxGitProxyRefLen bounds a branch/ref parameter. Git itself has no hard
	// limit, but filesystem-backed refs do, and an unbounded ref is a
	// pointless thing to accept from an agent.
	maxGitProxyRefLen = 255

	// maxGitProxyRemotes bounds what `git remote` listing we will walk. A repo
	// with more remotes than this is pathological; we report what we saw.
	maxGitProxyRemotes = 50
)

// gitProxyDisabledCode is the stable machine-readable code returned on every
// proxy route while the operator has configured no allow-list. It is 503
// rather than 404: the route exists, the capability is simply not turned on,
// and the agent should tell its human rather than retry.
const gitProxyDisabledCode = "git_proxy_disabled"

// gitProxyDisabledMessage is written to be actionable for an agent that has
// just been refused — it names the exact config the operator must add.
const gitProxyDisabledMessage = "the git/github proxy is not enabled: no remotes are allow-listed. " +
	"Ask the operator to set agent.git_proxy.allowed_remotes in ~/.tclaude/data/config.json " +
	`(e.g. {"agent":{"git_proxy":{"allowed_remotes":["github.com/your-org"]}}}).`

// ---------------------------------------------------------------------------
// Binary resolution
// ---------------------------------------------------------------------------

// proxyBinaries caches the absolute paths of git and gh, resolved once.
//
// Resolving once and invoking the ABSOLUTE path matters: exec.Command resolves
// a bare name against the DAEMON's PATH at call time, not against the
// constructed child environment, so a PATH entry that appears later in the
// daemon's lifetime would silently select a different binary.
var proxyBinaries struct {
	once sync.Once
	git  string
	gh   string
	errs map[string]error
}

func proxyBinary(name string) (string, error) {
	proxyBinaries.once.Do(func() {
		proxyBinaries.errs = map[string]error{}
		for _, n := range []string{"git", "gh"} {
			path, err := exec.LookPath(n)
			if err != nil {
				proxyBinaries.errs[n] = fmt.Errorf("%s is not installed on the host running agentd (%w)", n, err)
				continue
			}
			if abs, err := filepath.Abs(path); err == nil {
				path = abs
			}
			switch n {
			case "git":
				proxyBinaries.git = path
			case "gh":
				proxyBinaries.gh = path
			}
		}
	})
	switch name {
	case "git":
		if proxyBinaries.git == "" {
			return "", proxyBinaries.errs["git"]
		}
		return proxyBinaries.git, nil
	case "gh":
		if proxyBinaries.gh == "" {
			return "", proxyBinaries.errs["gh"]
		}
		return proxyBinaries.gh, nil
	}
	return "", fmt.Errorf("unknown proxy binary %q", name)
}

// SetProxyBinariesForTest pins the git/gh paths so a flow test never depends on
// what is installed on the runner. Returns a restore func for t.Cleanup.
func SetProxyBinariesForTest(gitPath, ghPath string) func() {
	proxyBinaries.once.Do(func() { proxyBinaries.errs = map[string]error{} })
	prevGit, prevGH := proxyBinaries.git, proxyBinaries.gh
	prevErrs := proxyBinaries.errs
	proxyBinaries.git, proxyBinaries.gh = gitPath, ghPath
	proxyBinaries.errs = map[string]error{}
	return func() {
		proxyBinaries.git, proxyBinaries.gh = prevGit, prevGH
		proxyBinaries.errs = prevErrs
	}
}

// ---------------------------------------------------------------------------
// The subprocess seam
// ---------------------------------------------------------------------------

// ProxyCommand is one fully-resolved subprocess. Everything about it is decided
// before it reaches the seam: the absolute binary path, the complete argv, the
// working directory, and the constructed environment. Nothing downstream may
// add to it.
type ProxyCommand struct {
	Tool string   // "git" | "gh" — diagnostics and audit only
	Path string   // absolute binary path
	Args []string // argv[1:]
	Dir  string   // working directory; always explicit, never inherited
	Env  []string // complete environment; NOT the daemon's
}

// ProxyResult is the bounded outcome of a proxied subprocess.
type ProxyResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
	TimedOut  bool
}

// proxyExec is the subprocess boundary, mirroring gitInfoResolver
// (branchlinks.go) and clipboardWrite (clipboard.go). Production runs the real
// command; flow tests swap in a recorder and assert on the exact argv and
// environment the daemon built — which is the only way to regression-test the
// hardening pins, since a missing pin produces no visible behaviour change
// until something malicious exploits it.
var proxyExec = runProxyCommand

// SetProxyExecForTest swaps the subprocess boundary. Returns a restore func.
func SetProxyExecForTest(fn func(ctx context.Context, cmd ProxyCommand) (ProxyResult, error)) func() {
	prev := proxyExec
	proxyExec = fn
	return func() { proxyExec = prev }
}

// runProxyCommand is the production seam implementation: bounded time, bounded
// output, and a private process group that is killed after Wait so an ssh or
// git-remote-https child cannot outlive the request.
func runProxyCommand(ctx context.Context, spec ProxyCommand) (ProxyResult, error) {
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	stdout := newProxyTail(gitProxyMaxOutputBytes)
	stderr := newProxyTail(gitProxyMaxOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	configureProxyCommand(cmd)

	runErr := cmd.Run()
	// Reap the private group unconditionally: an ordinary descendant that
	// outlived its group leader would otherwise keep running. A process that
	// deliberately escapes with setsid still needs a real sandbox to contain —
	// that is not this layer's job.
	_ = cleanupProxyCommand(cmd)

	res := ProxyResult{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Truncated: stdout.Truncated() || stderr.Truncated(),
	}
	if runErr == nil {
		return res, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			res.TimedOut = true
		}
		return res, nil
	}
	if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	return res, fmt.Errorf("run %s: %w", spec.Tool, runErr)
}

// proxyTail keeps the LAST maximum bytes written to it, flagging that it had to
// drop something. The tail is the useful end of git's output: the error, or the
// ref-update summary.
type proxyTail struct {
	maximum   int
	data      []byte
	truncated bool
}

func newProxyTail(maximum int) *proxyTail { return &proxyTail{maximum: maximum} }

func (b *proxyTail) Write(p []byte) (int, error) {
	written := len(p)
	if b.maximum <= 0 {
		b.truncated = b.truncated || written > 0
		return written, nil
	}
	if len(p) >= b.maximum {
		b.truncated = b.truncated || len(p) > b.maximum || len(b.data) > 0
		b.data = append(b.data[:0], p[len(p)-b.maximum:]...)
		return written, nil
	}
	if overflow := len(b.data) + len(p) - b.maximum; overflow > 0 {
		b.truncated = true
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, p...)
	return written, nil
}

func (b *proxyTail) String() string  { return strings.ToValidUTF8(string(b.data), "?") }
func (b *proxyTail) Truncated() bool { return b.truncated }

// ---------------------------------------------------------------------------
// The hardened git invocation
// ---------------------------------------------------------------------------

// gitProxyHooksDir returns a daemon-owned, permanently empty directory to point
// core.hooksPath at. It lives under the private data tree, which sandboxed
// agents cannot write, so an agent cannot plant a hook in it.
func gitProxyHooksDir() (string, error) {
	base := tclcommon.TclaudeDataDir()
	if base == "" {
		return "", errors.New("could not determine tclaude private data directory")
	}
	dir := filepath.Join(base, "gitproxy", "no-hooks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create empty hooks directory: %w", err)
	}
	return dir, nil
}

// gitProxyConfigPins are the `-c key=value` overrides prepended to EVERY git
// invocation the proxy makes. `-c` has the highest precedence in git's
// configuration order, so each of these beats the repo-local value an agent
// could have written.
//
// Read this list as a catalogue of the ways repo-local config can make git run
// a program or reach somewhere unintended.
func gitProxyConfigPins(hooksDir, sshCommand string, credentialHelpers []string) []string {
	pins := []string{
		// Command execution vectors.
		"core.hooksPath=" + hooksDir, // pre-push, reference-transaction, ...
		"core.fsmonitor=false",
		"core.alternateRefsCommand=",
		"core.sshCommand=" + sshCommand,
		"core.editor=false",
		"core.pager=cat",
		"gpg.program=false",
		"diff.external=",
		// Transport restriction. protocol.allow is the default for protocols
		// without a specific setting, so denying it and re-allowing exactly
		// https+ssh refuses ext:: (arbitrary command execution) and file://.
		"protocol.allow=never",
		"protocol.https.allow=always",
		"protocol.ssh.allow=always",
		"protocol.file.allow=never",
		"protocol.ext.allow=never",
		// Redirection.
		"http.proxy=",
		// Housekeeping: never start background maintenance inside an agent's
		// repository as a side effect of a proxied fetch.
		"gc.auto=0",
		"maintenance.auto=false",
		// Reset the credential-helper LIST, then re-add only the operator's
		// own global/system helpers below. An empty value is git's documented
		// way to clear previously-configured helpers, which is what drops a
		// repo-local one — an arbitrary command.
		"credential.helper=",
	}
	for _, helper := range credentialHelpers {
		pins = append(pins, "credential.helper="+helper)
	}
	out := make([]string, 0, len(pins)*2+1)
	for _, p := range pins {
		out = append(out, "-c", p)
	}
	return append(out, "--no-pager")
}

// gitProxySSHCommand renders the ssh transport. BatchMode=yes is the important
// part: without it a passphrase-protected key that is not already loaded into
// an agent would make ssh prompt, and the prompt would hang a daemon goroutine
// until the request timeout rather than failing with a usable message.
func gitProxySSHCommand(policy config.GitProxyConfig) string {
	parts := []string{"ssh", "-o", "BatchMode=yes"}
	if key := strings.TrimSpace(policy.SSHKey); key != "" {
		parts = append(parts, "-i", key, "-o", "IdentitiesOnly=yes")
	}
	return strings.Join(parts, " ")
}

// gitProxyEnv builds the child environment from scratch rather than filtering
// the daemon's. Constructing it is what guarantees that every GIT_CONFIG_*,
// GIT_SSH*, GIT_ASKPASS, GIT_EXTERNAL_DIFF, GIT_PROXY_COMMAND and GH_* value
// the daemon happens to hold is absent — a deny-list would have to be kept in
// sync with git forever, an allow-list does not.
func gitProxyEnv() []string {
	env := []string{
		// LC_ALL=C keeps git's porcelain-adjacent output stable for parsing.
		"LC_ALL=C",
		// Never prompt on the daemon's terminal for a username/password.
		"GIT_TERMINAL_PROMPT=0",
	}
	for _, name := range []string{
		"PATH",          // git needs its helper binaries (git-remote-https, ssh)
		"HOME",          // ~/.gitconfig, ~/.ssh/config — operator-owned
		"TMPDIR",        // git writes temporary pack files
		"SSH_AUTH_SOCK", // the preferred credential path: no secret in the child
	} {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// globalCredentialHelpers reads the operator's own credential helpers from
// GLOBAL and SYSTEM configuration.
//
// It deliberately runs in a neutral directory so no repository-local config is
// in scope: the whole point is to keep the operator's real helper working
// while dropping any helper an agent may have written into .git/config.
func globalCredentialHelpers(ctx context.Context, gitPath string) []string {
	neutral := os.TempDir()
	res, err := proxyExec(ctx, ProxyCommand{
		Tool: "git",
		Path: gitPath,
		// --global and --system are read separately by git; `git config
		// --get-all` without a scope flag would include the local repo, which
		// is exactly what must not be trusted here.
		Args: []string{"config", "--global", "--get-all", "credential.helper"},
		Dir:  neutral,
		Env:  gitProxyEnv(),
	})
	var helpers []string
	if err == nil && res.ExitCode == 0 {
		helpers = append(helpers, splitNonEmptyLines(res.Stdout)...)
	}
	res, err = proxyExec(ctx, ProxyCommand{
		Tool: "git",
		Path: gitPath,
		Args: []string{"config", "--system", "--get-all", "credential.helper"},
		Dir:  neutral,
		Env:  gitProxyEnv(),
	})
	if err == nil && res.ExitCode == 0 {
		helpers = append(helpers, splitNonEmptyLines(res.Stdout)...)
	}
	return helpers
}

func splitNonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if v := strings.TrimSpace(line); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// gitProxySession carries everything a single request needs: the resolved
// policy, the repository, and the pinned argv prefix.
type gitProxySession struct {
	policy   config.GitProxyConfig
	gitPath  string
	repoRoot string
	pins     []string
}

// newGitProxySession resolves the operator policy, the agent's repository and
// the hardened argv prefix for one request. remoteName may be empty for
// operations that name no remote.
func newGitProxySession(ctx context.Context, convID, remoteName string) (*gitProxySession, *proxyFault) {
	cfg, err := config.Load()
	if err != nil {
		return nil, faultf(500, "config", "could not read the daemon configuration: %v", err)
	}
	policy := cfg.ResolvedGitProxy()
	if len(policy.AllowedRemotes) == 0 {
		return nil, &proxyFault{Status: 503, Code: gitProxyDisabledCode, Msg: gitProxyDisabledMessage}
	}
	gitPath, err := proxyBinary("git")
	if err != nil {
		return nil, faultf(503, "tool_missing", "%v", err)
	}
	hooksDir, err := gitProxyHooksDir()
	if err != nil {
		return nil, faultf(500, "io", "%v", err)
	}
	repoRoot, fault := resolveProxyRepo(ctx, gitPath, convID)
	if fault != nil {
		return nil, fault
	}
	pins := gitProxyConfigPins(hooksDir, gitProxySSHCommand(policy),
		globalCredentialHelpers(ctx, gitPath))
	return &gitProxySession{policy: policy, gitPath: gitPath, repoRoot: repoRoot, pins: pins}, nil
}

// gitProxyUploadPack / gitProxyReceivePack are the stock transport programs.
//
// They are passed as COMMAND-LINE FLAGS rather than `-c remote.<n>.uploadpack=`
// overrides, and that distinction is load-bearing rather than stylistic: git
// reads these two keys first-wins across scopes, so a repo-local value BEATS a
// `-c` override ("more than one uploadpack given, using the first"). The flags
// do override it. Verified against git 2.x; see TestGitProxy_*ArgvIsHardened.
const (
	gitProxyUploadPack  = "--upload-pack=git-upload-pack"
	gitProxyReceivePack = "--receive-pack=git-receive-pack"
)

// dangerousRemoteKeys are the per-remote configuration keys that select a
// PROGRAM rather than describe a destination. The proxy refuses outright when a
// repository sets one, instead of trying to neutralize it.
//
// Refusal rather than override is deliberate. `uploadpack` and `receivepack`
// are first-wins keys that a `-c` override cannot displace at all; `vcs`
// selects a `git-remote-<value>` helper binary; `proxy` redirects the
// connection. Some are overridable and some are not, and that asymmetry is a
// property of git's config reader that could change. A repository that sets any
// of them is doing something the proxy has no reason to support, so the honest
// answer is to stop rather than to guess which neutralization still works.
var dangerousRemoteKeys = []string{"uploadpack", "receivepack", "vcs", "proxy"}

// refuseDangerousRemoteConfig fails closed when the agent's repository
// configures a program-selecting key for the named remote.
//
// It reads the EFFECTIVE value (not `--local`), so a value arriving through an
// include, or through the operator's own global config, is caught too. That is
// the conservative direction: an operator who has genuinely set
// remote.origin.uploadpack globally gets a clear refusal rather than a silently
// different transport program.
func refuseDangerousRemoteConfig(ctx context.Context, s *gitProxySession, remoteName string) *proxyFault {
	for _, key := range dangerousRemoteKeys {
		full := "remote." + remoteName + "." + key
		if value := s.gitProbe(ctx, "config", "--get", "--", full); value != "" {
			return faultf(http.StatusForbidden, "remote_config_refused",
				"this repository sets %s, which selects a program rather than a destination; "+
					"the proxy refuses to run against a remote configured that way", full)
		}
	}
	return nil
}

// git runs a hardened git command in the agent's repository.
func (s *gitProxySession) git(ctx context.Context, args ...string) (ProxyResult, error) {
	return proxyExec(ctx, ProxyCommand{
		Tool: "git",
		Path: s.gitPath,
		Args: append(append([]string(nil), s.pins...), args...),
		Dir:  s.repoRoot,
		Env:  gitProxyEnv(),
	})
}

// gitProbe runs a local git command and returns its trimmed stdout, treating a
// non-zero exit as "no answer" rather than an error. Probes are questions about
// the repository ("what is the current branch?"), and an unanswerable question
// is handled by the caller, not surfaced as a subprocess failure.
func (s *gitProxySession) gitProbe(ctx context.Context, args ...string) string {
	probeCtx, cancel := context.WithTimeout(ctx, gitProxyProbeTimeout)
	defer cancel()
	res, err := s.git(probeCtx, args...)
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// ---------------------------------------------------------------------------
// The repo gate — "where may this run"
// ---------------------------------------------------------------------------

// resolveProxyRepo decides which repository a proxied operation runs in.
//
// It takes NO path from the agent. The repository is the git work-tree root
// containing the agent's own daemon-recorded physical launch directory —
// sessions.resume_provenance, the immutable value captured at launch, falling
// back to sessions.cwd only for pre-provenance legacy rows.
//
// This is the same "the trusted selector is daemon state, not a request
// parameter" shape `agent dir --repair` uses, and it is what makes the
// lend-credentials-not-reach invariant hold by construction: there is no
// parameter through which an agent could aim the daemon's credentials at a
// repository that is not its own.
//
// It deliberately does NOT consult agent_workdir. That table is written by the
// agent's own PostToolUse hook, so it is agent-influenced; treating it as
// authority would hand an agent exactly the aiming primitive this function
// exists to withhold.
func resolveProxyRepo(ctx context.Context, gitPath, convID string) (string, *proxyFault) {
	sess, err := db.FindSessionByConvID(convID)
	if err != nil {
		return "", faultf(500, "repo_unresolved", "could not load your session record: %v", err)
	}
	if sess == nil || strings.TrimSpace(sess.Cwd) == "" {
		return "", faultf(404, "repo_unresolved",
			"no recorded launch directory for %s — is this session running under tclaude?", short8(convID))
	}
	launchDir, err := recordedLaunchDir(sess)
	if err != nil {
		return "", faultf(409, "repo_unresolved", "%v", err)
	}
	if !filepath.IsAbs(launchDir) {
		return "", faultf(409, "repo_unresolved",
			"the recorded launch directory is not absolute; refusing to run git against it")
	}
	// Resolve symlinks before asking git anything, so the path we hand the
	// subprocess is the physical one and cannot be re-aimed by swapping a link
	// between the check and the use.
	resolved, err := filepath.EvalSymlinks(launchDir)
	if err != nil {
		return "", faultf(409, "repo_unresolved",
			"the recorded launch directory %s is not reachable: %v", launchDir, err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, gitProxyProbeTimeout)
	defer cancel()
	res, err := proxyExec(probeCtx, ProxyCommand{
		Tool: "git",
		Path: gitPath,
		Args: []string{"-C", resolved, "rev-parse", "--path-format=absolute", "--show-toplevel"},
		Dir:  resolved,
		Env:  gitProxyEnv(),
	})
	if err != nil {
		return "", faultf(500, "repo_unresolved", "could not inspect the repository: %v", err)
	}
	root := strings.TrimSpace(res.Stdout)
	if res.ExitCode != 0 || root == "" {
		return "", faultf(409, "not_a_repo",
			"your launch directory %s is not inside a git work tree", resolved)
	}
	if !filepath.IsAbs(root) {
		return "", faultf(500, "repo_unresolved",
			"git reported a non-absolute work-tree root %q", root)
	}
	return filepath.Clean(root), nil
}

// recordedLaunchDir returns the immutable physical launch directory recorded
// for a session. It mirrors recordedStartupDir (dir.go) — the physical path
// matters when sessions.cwd named a symlink that was later retargeted.
func recordedLaunchDir(sess *db.SessionRow) (string, error) {
	raw := strings.TrimSpace(sess.ResumeProvenance)
	if raw == "" {
		return strings.TrimSpace(sess.Cwd), nil
	}
	prov, err := resumeprovenance.Decode(raw)
	if err != nil {
		return "", fmt.Errorf("decode recorded launch provenance: %w", err)
	}
	return strings.TrimSpace(prov.Cwd.Path), nil
}

// ---------------------------------------------------------------------------
// The remote gate — "what may this talk to"
// ---------------------------------------------------------------------------

// remoteRef is a parsed, validated remote destination.
type remoteRef struct {
	Scheme string   // "https" | "ssh"
	Host   string   // lower-cased DNS host, no port
	Path   []string // path segments, ".git" suffix stripped
}

// Key is the lower-cased "host/a/b/c" form the allow-list matches against.
func (r remoteRef) Key() string {
	return strings.ToLower(r.Host + "/" + strings.Join(r.Path, "/"))
}

// OwnerRepo is the "owner/repo" form `gh --repo` wants. For a forge with
// nested groups it uses the first and last segments, which is what GitHub
// needs and is meaningless (but harmless) elsewhere.
func (r remoteRef) OwnerRepo() string {
	if len(r.Path) < 2 {
		return ""
	}
	return r.Path[0] + "/" + r.Path[len(r.Path)-1]
}

// parseRemoteURL parses a git remote URL and refuses everything that is not a
// plain https or ssh destination.
//
// The refusals are the point of this function, so they are explicit rather
// than falling out of a failed parse:
//
//   - "ext::<command>" hands git a command line to execute. It is the single
//     sharpest edge in a repository an agent can write.
//   - "file://" and bare local paths escape the allow-list entirely — the
//     allow-list is about hosts, and a local path has none.
//   - "http://" would send a credential in clear text.
//   - "git://" is unauthenticated and unencrypted; there is no reason for the
//     credential proxy to speak it.
//   - A leading "-" would make the URL parse as a flag at the argv level.
//   - Embedded userinfo with a password is a credential in the repo config,
//     which we neither want to use nor to leak into an error message.
func parseRemoteURL(raw string) (remoteRef, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return remoteRef{}, errors.New("the remote has no URL configured")
	}
	if strings.HasPrefix(s, "-") {
		return remoteRef{}, errors.New("refusing a remote URL that begins with '-'")
	}
	// The transport check comes BEFORE the charset check on purpose. An
	// "ext::sh -c …" URL would also trip the whitespace rule, but "contains
	// whitespace" is a useless thing to tell an agent that has just been
	// pointed at a command-execution transport — name the real hazard.
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "ext::"):
		return remoteRef{}, errors.New(
			"refusing an 'ext::' remote URL: it names a command for git to execute, not a server")
	case strings.HasPrefix(lower, "file://"), strings.HasPrefix(lower, "/"), strings.HasPrefix(lower, "./"),
		strings.HasPrefix(lower, "../"):
		return remoteRef{}, errors.New(
			"refusing a local-path remote: the proxy exists to reach network remotes, and a local path has no host to allow-list")
	case strings.HasPrefix(lower, "http://"):
		return remoteRef{}, errors.New("refusing an http:// remote: credentials must not travel in clear text")
	case strings.HasPrefix(lower, "git://"):
		return remoteRef{}, errors.New("refusing a git:// remote: it is neither authenticated nor encrypted")
	}
	if strings.ContainsAny(s, " \t\r\n\x00") {
		return remoteRef{}, errors.New("refusing a remote URL containing whitespace or control characters")
	}

	if strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "ssh://") {
		u, err := url.Parse(s)
		if err != nil {
			return remoteRef{}, fmt.Errorf("the remote URL could not be parsed: %w", err)
		}
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				return remoteRef{}, errors.New("refusing a remote URL with an embedded password")
			}
		}
		if u.Port() != "" {
			// The allow-list has no way to express a port, so accepting one
			// would mean silently authorizing a destination the operator
			// never wrote down. Refusing is the honest answer; if a forge on
			// a non-default port is genuinely needed, that is a change to the
			// allow-list grammar, not something to wave through here.
			return remoteRef{}, fmt.Errorf(
				"refusing a remote URL with an explicit port (%q): the allow-list matches host/owner/repo only", u.Port())
		}
		scheme := "https"
		if strings.EqualFold(u.Scheme, "ssh") {
			scheme = "ssh"
		}
		return buildRemoteRef(scheme, u.Hostname(), u.Path)
	}

	// scp-like syntax: [user@]host:path — note this is only reached after the
	// scheme checks above, so "ext::…" can never land here.
	if at := strings.LastIndex(s, "@"); at >= 0 {
		if strings.Contains(s[:at], ":") {
			return remoteRef{}, errors.New("refusing a remote URL with an embedded password")
		}
		s = s[at+1:]
	}
	colon := strings.Index(s, ":")
	if colon <= 0 || colon == len(s)-1 {
		return remoteRef{}, errors.New(
			"unrecognised remote URL form; expected https://host/owner/repo, ssh://host/owner/repo, or host:owner/repo")
	}
	return buildRemoteRef("ssh", s[:colon], s[colon+1:])
}

// remoteHostPattern bounds what we accept as a DNS host: letters, digits,
// hyphens and dots. Anything else — a port, an IP literal in brackets, a
// percent-escape — is refused rather than normalised, because the allow-list
// match must be a plain string comparison to be auditable.
func validRemoteHost(host string) bool {
	if host == "" || len(host) > 253 || strings.HasPrefix(host, "-") || strings.HasPrefix(host, ".") ||
		strings.HasSuffix(host, ".") {
		return false
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

func buildRemoteRef(scheme, host, path string) (remoteRef, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if !validRemoteHost(host) {
		return remoteRef{}, fmt.Errorf("refusing a remote whose host %q is not a plain DNS name", host)
	}
	path = strings.Trim(strings.TrimSpace(path), "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" {
		return remoteRef{}, errors.New("the remote URL names no repository path")
	}
	var segs []string
	for _, seg := range strings.Split(path, "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "." || seg == ".." {
			return remoteRef{}, fmt.Errorf("refusing a remote path containing %q", seg)
		}
		segs = append(segs, seg)
	}
	if len(segs) < 2 {
		return remoteRef{}, errors.New("the remote URL does not name an owner and repository")
	}
	return remoteRef{Scheme: scheme, Host: host, Path: segs}, nil
}

// remoteAllowed reports whether ref matches any operator allow-list pattern.
// Patterns are slash-separated; "*" matches exactly one segment, and a pattern
// with fewer segments than the target matches as a prefix — so "github.com"
// covers every repo on that host and "github.com/acme" covers one owner.
func remoteAllowed(ref remoteRef, patterns []string) bool {
	target := strings.Split(ref.Key(), "/")
	for _, pattern := range patterns {
		if matchRemotePattern(strings.Split(pattern, "/"), target) {
			return true
		}
	}
	return false
}

func matchRemotePattern(pattern, target []string) bool {
	if len(pattern) == 0 || len(pattern) > len(target) {
		return false
	}
	for i, seg := range pattern {
		if seg == "*" {
			continue
		}
		if seg != target[i] {
			return false
		}
	}
	return true
}

// resolvedRemote is a remote that has passed every gate.
type resolvedRemote struct {
	Name     string
	FetchRef remoteRef
	PushRef  remoteRef
	FetchURL string
	PushURL  string
}

// resolveProxyRemote validates a remote name end to end.
//
// Both the fetch URL and the PUSH url are validated: remote.<name>.pushurl can
// differ from remote.<name>.url, so validating only the latter would leave
// push aimed somewhere unchecked.
//
// Each URL is then required to be a FIXED POINT of git's own URL rewriting
// (`git ls-remote --get-url <url>` returns the same string). That is how
// url.<base>.insteadOf is defeated: it is the one dangerous key that a `-c`
// override cannot reset, so instead of trying to disable it we require that it
// does not apply. A repository configured to rewrite the URL we validated is
// refused rather than followed somewhere we never checked.
func resolveProxyRemote(ctx context.Context, s *gitProxySession, name string) (resolvedRemote, *proxyFault) {
	if fault := validateRemoteName(name); fault != nil {
		return resolvedRemote{}, fault
	}
	if fault := refuseDangerousRemoteConfig(ctx, s, name); fault != nil {
		return resolvedRemote{}, fault
	}
	fetchURL := s.gitProbe(ctx, "remote", "get-url", "--", name)
	if fetchURL == "" {
		return resolvedRemote{}, faultf(404, "unknown_remote",
			"no remote named %q is configured in %s", name, filepath.Base(s.repoRoot))
	}
	pushURL := s.gitProbe(ctx, "remote", "get-url", "--push", "--", name)
	if pushURL == "" {
		pushURL = fetchURL
	}

	out := resolvedRemote{Name: name, FetchURL: fetchURL, PushURL: pushURL}
	var err error
	if out.FetchRef, err = parseRemoteURL(fetchURL); err != nil {
		return resolvedRemote{}, faultf(403, "remote_refused", "remote %q fetch URL: %v", name, err)
	}
	if out.PushRef, err = parseRemoteURL(pushURL); err != nil {
		return resolvedRemote{}, faultf(403, "remote_refused", "remote %q push URL: %v", name, err)
	}
	for _, check := range []struct {
		label string
		url   string
		ref   remoteRef
	}{
		{"fetch", fetchURL, out.FetchRef},
		{"push", pushURL, out.PushRef},
	} {
		if !remoteAllowed(check.ref, s.policy.AllowedRemotes) {
			return resolvedRemote{}, faultf(403, "remote_not_allowed",
				"remote %q (%s %s) is not on the operator's allow-list; allowed: %s",
				name, check.label, check.ref.Key(), strings.Join(s.policy.AllowedRemotes, ", "))
		}
		if rewritten := s.gitProbe(ctx, "ls-remote", "--get-url", "--", check.url); rewritten != check.url {
			return resolvedRemote{}, faultf(403, "remote_rewritten",
				"this repository rewrites its %s URL (url.*.insteadOf); refusing to follow a redirect that was not validated",
				check.label)
		}
	}
	return out, nil
}

// validateRemoteName bounds a remote name. Git's own rules are looser, but a
// remote name reaches argv and is interpolated into the `remote.<name>.…`
// config keys refuseDangerousRemoteConfig probes, so it is held to a
// conservative charset and may never begin with "-".
func validateRemoteName(name string) *proxyFault {
	if name == "" {
		return faultf(400, "invalid_arg", "a remote name is required")
	}
	if len(name) > 100 {
		return faultf(400, "invalid_arg", "remote name is too long")
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") {
		return faultf(400, "invalid_arg", "a remote name may not begin with '-' or '.'")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return faultf(400, "invalid_arg",
				"remote name %q contains characters outside [A-Za-z0-9._-]", name)
		}
	}
	return nil
}

// validateBranchName applies git's check-ref-format rules to a branch name
// locally, so a malformed or hostile value is refused before it reaches argv
// rather than after. The leading-"-" rule is the security-relevant one; the
// rest keep the error message useful.
func validateBranchName(branch string) *proxyFault {
	if branch == "" {
		return faultf(400, "invalid_arg", "a branch name is required")
	}
	if len(branch) > maxGitProxyRefLen {
		return faultf(400, "invalid_arg", "branch name is longer than %d characters", maxGitProxyRefLen)
	}
	if strings.HasPrefix(branch, "-") {
		return faultf(400, "invalid_arg", "a branch name may not begin with '-'")
	}
	if strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") || strings.Contains(branch, "//") {
		return faultf(400, "invalid_arg", "a branch name may not begin, end, or contain an empty path segment")
	}
	if strings.HasSuffix(branch, ".") || strings.HasSuffix(branch, ".lock") ||
		strings.Contains(branch, "..") || strings.Contains(branch, "@{") {
		return faultf(400, "invalid_arg", "branch name %q is not a valid git ref", branch)
	}
	if branch == "@" || branch == "HEAD" {
		return faultf(400, "invalid_arg", "%q is not a branch this proxy will act on", branch)
	}
	for _, r := range branch {
		if r < 0x20 || r == 0x7f {
			return faultf(400, "invalid_arg", "branch name contains a control character")
		}
		switch r {
		case ' ', '~', '^', ':', '?', '*', '[', '\\':
			return faultf(400, "invalid_arg", "branch name %q contains the reserved character %q", branch, r)
		}
	}
	return nil
}

// refProtected reports whether branch matches an operator-protected pattern.
// "*" matches within a segment, so "release/*" protects a namespace.
func refProtected(branch string, patterns []string) bool {
	lower := strings.ToLower(branch)
	for _, p := range patterns {
		if matchRefPattern(p, lower) {
			return true
		}
	}
	return false
}

func matchRefPattern(pattern, branch string) bool {
	if pattern == branch {
		return true
	}
	star := strings.IndexByte(pattern, '*')
	if star < 0 {
		return false
	}
	prefix, suffix := pattern[:star], pattern[star+1:]
	if !strings.HasPrefix(branch, prefix) || !strings.HasSuffix(branch, suffix) {
		return false
	}
	return len(branch) >= len(prefix)+len(suffix)
}

// ---------------------------------------------------------------------------
// Faults
// ---------------------------------------------------------------------------

// proxyFault is a typed refusal carrying the HTTP status, the stable code and
// an agent-readable explanation. Handlers return it rather than writing the
// response inline, so every gate reads as a single expression and the
// permission/validation order stays visible.
type proxyFault struct {
	Status int
	Code   string
	Msg    string
}

func faultf(status int, code, format string, args ...any) *proxyFault {
	return &proxyFault{Status: status, Code: code, Msg: fmt.Sprintf(format, args...)}
}

// writeProxyFault renders a fault onto the response.
func writeProxyFault(w http.ResponseWriter, f *proxyFault) {
	writeError(w, f.Status, f.Code, f.Msg)
}
