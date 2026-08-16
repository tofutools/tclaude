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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/resumeprovenance"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// gitproxy.go is the daemon half of `tclaude proxy git` — a *semantic* proxy
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
//     reference-transaction, the last of which fires on ref updates — which is
//     what makes it the one that matters for the fetch ref import, the only
//     command this proxy still runs inside the agent's repository.
//   - Only network-half operations run here — fetch, push, ls-remote. None of
//     them updates the working tree, so .gitattributes smudge/clean filters
//     never execute. `tclaude proxy git pull` is deliberately split: the
//     daemon fetches, and the fast-forward runs in the agent's own process
//     where it needs no credential. See the CLI side.
//   - All three of them run their CREDENTIALED command from a daemon-owned
//     transfer directory rather than from the agent's repository, so the
//     configuration above is not merely distrusted but out of scope. See
//     gitProxyXfer, and gitproxy_refs.go for the extra work fetch needs to
//     leave its results behind.
//   - The remote URL is validated rather than trusted. `ext::` URLs execute a
//     command outright and `file:` escapes the allow-list, so the transport
//     is pinned to https/ssh at the git level AND the parsed URL is
//     independently required to be one of those two.
//   - core.askPass names a program git runs to obtain a credential, and it is
//     consulted BEFORE the terminal — so GIT_TERMINAL_PROMPT=0 does not close
//     it. It is pinned empty. core.sshCommand, core.alternateRefsCommand,
//     core.fsmonitor, core.gitProxy, core.editor, core.pager, gpg.program and
//     diff.external are pinned for the same reason.
//   - Submodule recursion would dial a host named by a submodule's own
//     configuration, which the allow-list never inspected. Pinned off and
//     passed --no-recurse-submodules.
//   - credential.helper is reset and then re-populated from GLOBAL/SYSTEM
//     configuration only, so an operator's real helper keeps working while a
//     repo-local one — an arbitrary command — is dropped.
//
// PINNING IS NOT UNIFORMLY RELIABLE, which shapes everything above. Three
// separate classes of key turned out to resist `-c`:
//
//   - remote.<name>.uploadpack / receivepack are read FIRST-WINS across
//     scopes, so a repo-local value beats the command line outright ("more
//     than one uploadpack given, using the first").
//   - http.<url>.* entries beat a generic `-c http.proxy=` by URL specificity,
//     and http.sslVerify / sslCAInfo / curloptResolve have no generic form
//     worth pinning at all.
//   - url.<base>.insteadOf has no reset form.
//
// So the load-bearing mechanism is REFUSAL, not neutralization:
// refuseHostileRepoConfig rejects the whole operation when the repository
// carries any of those families (see dangerousRemoteKeys / safeHTTPKeys), and
// resolveProxyRemote requires each validated URL to be a fixed point of git's
// own rewriting. The stock transport programs additionally ride as
// --upload-pack / --receive-pack FLAGS, which DO override config.
//
// One more consequence of not trusting the repository: `git remote get-url`
// reports only the FIRST url, while `git push` contacts EVERY configured one.
// Every URL is validated, via --all.
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

	// maxGitProxyMirrorRefs bounds a ref namespace the fetch MIRRORS between
	// the transfer directory and the agent's repository. Truncation here is
	// not survivable — a ref that exists locally but fell off the listing would
	// be imported as a creation and the transaction would fail with a lock
	// error naming nothing useful — so the listing asks for one more than this
	// and refuses when it gets it. 20k refs is far past any working checkout.
	maxGitProxyMirrorRefs = 20000

	// maxGitProxyHaveRefs bounds the agent's OTHER refs, which are copied into
	// the transfer directory purely so fetch negotiation can say "I already
	// have this". Truncating those costs bandwidth and nothing else, so this
	// one is a plain cap with no refusal attached.
	maxGitProxyHaveRefs = 2000

	// maxGitProxyRefBytes is the output bound for a ref listing. proxyTail keeps
	// the TAIL, so an exceeded bound drops the FIRST refs — which is why every
	// caller treats Truncated as a refusal rather than as a short answer.
	maxGitProxyRefBytes = 4 * 1024 * 1024

	// gitProxyXferMaxAge is how long an abandoned transfer directory may sit
	// under the private data tree before the next request removes it. Every
	// transfer cleans up after itself; this only catches the ones whose daemon
	// was killed mid-fetch.
	gitProxyXferMaxAge = 6 * time.Hour
)

// haveRefNamespace is where the agent's own branches are parked inside the
// transfer directory. They exist solely as negotiation "have"s, so the
// credentialed fetch asks the server for what the repository is actually
// missing. Nothing is ever read back out of this namespace, and no refspec
// points into it, so `--prune` never considers it.
const haveRefNamespace = "refs/tclaude-have/"

// gitProxyDisabledCode is the stable machine-readable code returned when an
// unscoped grant has no legacy operator-global allow-list. A remote-scoped
// grant supplies its own patterns and does not take this path.
const gitProxyDisabledCode = "git_proxy_disabled"

func gitProxyRoutesEnabled(r *http.Request) bool {
	cfg, err := config.Load()
	if err == nil && cfg.GitProxyEnabled() {
		return true
	}
	p := peerFromContext(r.Context())
	if classify(p) != classAgent {
		return false
	}
	for _, slug := range []string{PermGitRead, PermGitPush, PermGitHubRead, PermGitHubWrite, PermGitHubMerge} {
		v := resolvePermissionVerdictForRequest(r, p.ConvID, slug)
		if v.Resolution == permAllow && !evalPermissionScope(v, p.ConvID, ActionContext{}).Unscoped {
			return true
		}
	}
	return false
}

// gitProxyDisabledMessage is written to be actionable for an unscoped grant.
const gitProxyDisabledMessage = "the git/github proxy has no remote policy for this unscoped grant. " +
	"Ask the operator to scope the grant by remote, or set legacy agent.git_proxy.allowed_remotes in ~/.tclaude/data/config.json " +
	`(e.g. {"agent":{"git_proxy":{"allowed_remotes":["github.com/your-org"]}}}).`

// ---------------------------------------------------------------------------
// Binary resolution
// ---------------------------------------------------------------------------

// proxyBinaries caches git's absolute path, resolved once.
//
// Resolving once and invoking the ABSOLUTE path matters: exec.Command resolves
// a bare name against the DAEMON's PATH at call time, not against the
// constructed child environment, so a PATH entry that appears later in the
// daemon's lifetime would silently select a different binary.
//
// git is the only binary either proxy runs. The GitHub half calls the API
// directly (githubapi.go), so `gh` is no longer a requirement on the host —
// its absence costs nothing unless the operator relies on it as a credential
// source of last resort.
var proxyBinaries struct {
	once sync.Once
	git  string
	err  error
}

func proxyBinary(name string) (string, error) {
	if name != "git" {
		return "", fmt.Errorf("unknown proxy binary %q", name)
	}
	proxyBinaries.once.Do(func() {
		path, err := exec.LookPath("git")
		if err != nil {
			proxyBinaries.err = fmt.Errorf("git is not installed on the host running agentd (%w)", err)
			return
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		proxyBinaries.git = path
	})
	if proxyBinaries.git == "" {
		return "", proxyBinaries.err
	}
	return proxyBinaries.git, nil
}

// SetProxyBinariesForTest pins git's path so a test never depends on what is
// installed on the runner. Returns a restore func for t.Cleanup.
//
// It resolves for real FIRST rather than consuming the sync.Once with a no-op.
// A no-op would mark lazy resolution as done while leaving the cache empty, so
// the restore would put back ("", nil) — an empty path with NO error, which
// every caller reads as success and then execs. Resolving first means the
// value restored is the one lazy resolution would have produced anyway.
//
// The error is discarded on purpose: a runner without git is a legitimate
// place to pin a fake path, and proxyBinary has already recorded the failure
// for the restore to hand back.
func SetProxyBinariesForTest(gitPath string) func() {
	_, _ = proxyBinary("git")
	prevGit, prevErr := proxyBinaries.git, proxyBinaries.err
	proxyBinaries.git, proxyBinaries.err = gitPath, nil
	return func() {
		proxyBinaries.git, proxyBinaries.err = prevGit, prevErr
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

	// Stdin is fed to the child, which is how `update-ref --stdin` receives a
	// whole ref transaction as one atomic unit rather than as one subprocess
	// per ref. It is daemon-authored in every case: no agent-supplied string
	// reaches it unvalidated, for the same reason none reaches Args.
	Stdin string

	// MaxOutputBytes overrides the per-stream tail kept from this command.
	// Zero means gitProxyMaxOutputBytes, which suits every command whose
	// output is a diagnosis. A verb whose output IS the payload — a CI log,
	// a comment thread — sets its own, larger bound.
	MaxOutputBytes int
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
// output, and a private process group that is killed on cancellation so an ssh
// or git-remote-https child cannot outlive the request.
func runProxyCommand(ctx context.Context, spec ProxyCommand) (ProxyResult, error) {
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	maxOutput := spec.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = gitProxyMaxOutputBytes
	}
	stdout := newProxyTail(maxOutput)
	stderr := newProxyTail(maxOutput)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if spec.Stdin != "" {
		cmd.Stdin = strings.NewReader(spec.Stdin)
	}
	configureProxyCommand(cmd)

	// Run, not Start/kill/Wait, and deliberately NO group kill afterwards.
	//
	// Wait reaps the group leader, and a reaped pid is immediately available
	// for reuse — so a kill(-pid) issued after Wait can land on an unrelated
	// process group that happens to have been allocated that id. The group kill
	// therefore lives only on the cancellation path, where os/exec invokes
	// cmd.Cancel while the leader is still unreaped (configureProxyCommand).
	//
	// A descendant that outlives a leader which exited normally is bounded
	// instead by WaitDelay: it holds the inherited stdout/stderr pipe, Wait
	// blocks on the pipe copy, and os/exec force-closes it after proxyWaitDelay.
	// Chasing it with a signal is not worth signalling a stranger's process
	// group; a process that deliberately escapes still needs a real OS sandbox
	// to contain, which is not this layer's job.
	runErr := cmd.Run()

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

// gitProxyXfer is a throwaway, DAEMON-OWNED git directory that a credentialed
// transfer runs from, so the agent's repository configuration is not in scope
// for the one command that carries the operator's credential.
//
// This is what closes the check/use race. Every refusal gate reads the agent's
// `.git/config` in its own short-lived process; the credentialed command used
// to read it again, moments later, and the agent can rewrite that file in
// between. Pins ride on the argv and are immune, but the keys that matter most
// here cannot be pinned: `url.*.insteadOf` has no reset form, and a URL-scoped
// `http.<url>.*` outranks a generic override. Verified on git 2.43 for both
// directions — a push and a fetch run from the agent's repo with a hostile
// insteadOf go to the attacker's host, and the same operations run from a
// directory like this one do not (TestRealGit_TransferDirIgnoresAgentInsteadOf,
// TestRealGit_FetchIsolatesTheCredentialedHalfAndImportsRefs).
//
// How it reaches the agent's objects depends on the verb, and the difference is
// the whole reason fetch took longer to convert than push:
//
//   - PUSH and LS-REMOTE only ever READ objects, so the agent's store is
//     borrowed through `objects/info/alternates`. Nothing is copied and nothing
//     is written back.
//   - FETCH has to LEAVE objects behind, and alternates only point one way. So
//     the transfer directory's object store IS the agent's, named through
//     GIT_OBJECT_DIRECTORY, and the fetched pack lands exactly where an
//     ordinary `git fetch` would have put it. Refs are the only thing that
//     still has to be moved afterwards — see seedRefs / importRefs.
//
// Either way nothing in the transfer directory is agent-writable: it lives
// under the private data tree.
type gitProxyXfer struct {
	dir string
	// objectDir, when set, is the object store git reads AND writes. Empty
	// means the alternates arrangement above.
	//
	// Its value is whatever the agent's repository answers for `rev-parse
	// --git-path objects`, so an agent that redirects that path — a symlinked
	// `.git/objects`, a `.git/commondir` gitfile — aims the daemon at the
	// target. Say plainly what that means, because it is NOT the same in the
	// two modes: alternates only ever READ through the agent-named path, while
	// this one has the daemon WRITE there, as the unsandboxed operator.
	//
	// It is deliberately left as it is. An in-repo `git fetch` — what this
	// replaced — wrote to exactly the same place, so it is not a regression;
	// and it is a poor primitive, since git names packs by content hash, the
	// agent's own repository is destroyed in the process, and nothing is read
	// back. Constraining it to the git common dir would break operators who
	// legitimately relocate an object store, which is a worse trade than the
	// narrowing buys.
	objectDir string

	// seeded is what the agent's repository held in the mirrored namespaces
	// when the transfer directory was built, retained as the baseline the
	// import compares and compare-and-swaps against. See importRefs.
	seeded   []gitRef
	seedDone bool
}

// xferObjectMode selects between the two arrangements described on
// gitProxyXfer.
type xferObjectMode int

const (
	// xferBorrowObjects reads the agent's objects and writes none back.
	xferBorrowObjects xferObjectMode = iota
	// xferShareObjects writes fetched objects straight into the agent's store.
	xferShareObjects
)

// newGitProxyXfer builds the transfer directory and connects it to the agent's
// object store in the requested direction.
func newGitProxyXfer(ctx context.Context, s *gitProxySession, mode xferObjectMode) (*gitProxyXfer, *proxyFault) {
	base := tclcommon.TclaudeDataDir()
	if base == "" {
		return nil, faultf(500, "io", "could not determine tclaude private data directory")
	}
	root := filepath.Join(base, "gitproxy", "xfer")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, faultf(500, "io", "create transfer directory: %v", err)
	}
	sweepStaleXferDirs(root)
	dir, err := os.MkdirTemp(root, "x")
	if err != nil {
		return nil, faultf(500, "io", "create transfer directory: %v", err)
	}
	x := &gitProxyXfer{dir: dir}

	// The agent's object store. Asked of the agent's repo, which is the only
	// thing we still need from it — objects, never configuration.
	objects, exit, ran := s.gitProbeStrict(ctx, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if !ran || exit != 0 || objects == "" {
		x.cleanup()
		return nil, faultf(500, "repo_unresolved",
			"could not locate this repository's object store; refusing to run a credentialed "+
				"transfer without one")
	}
	res, err := proxyExec(ctx, ProxyCommand{
		Tool: "git", Path: s.gitPath,
		Args: []string{"init", "--bare", "-q", dir},
		Dir:  root, Env: gitProxyEnv(),
	})
	if err != nil || res.ExitCode != 0 {
		x.cleanup()
		return nil, faultf(500, "io", "could not prepare the transfer directory (git init: %v)", err)
	}
	if mode == xferShareObjects {
		x.objectDir = objects
		return x, nil
	}
	if err := os.WriteFile(filepath.Join(dir, "objects", "info", "alternates"),
		[]byte(objects+"\n"), 0o600); err != nil {
		x.cleanup()
		return nil, faultf(500, "io", "could not borrow the repository's objects: %v", err)
	}
	return x, nil
}

// sweepStaleXferDirs removes transfer directories left behind by a daemon that
// died mid-transfer. Every transfer removes its own on the way out, so anything
// older than gitProxyXferMaxAge is debris. Best effort by design: a directory
// that cannot be read or removed is skipped rather than failing the request
// that happens to be passing through.
func sweepStaleXferDirs(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-gitProxyXferMaxAge)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "x") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, entry.Name()))
	}
}

func (x *gitProxyXfer) cleanup() {
	if x != nil && x.dir != "" {
		_ = os.RemoveAll(x.dir)
	}
}

// env is gitProxyEnv plus the one GIT_* variable the daemon sets deliberately.
// gitProxyEnv exists to guarantee no INHERITED GIT_* value reaches the child;
// a daemon-chosen one is the opposite case, and it is what lets a fetch write
// into the agent's object store without ever reading the agent's config.
func (x *gitProxyXfer) env() []string {
	env := gitProxyEnv()
	if x.objectDir != "" {
		env = append(env, "GIT_OBJECT_DIRECTORY="+x.objectDir)
	}
	return env
}

// git runs a hardened git command FROM the transfer directory. The agent's
// repository is never the working directory and never supplies --git-dir, so
// none of its configuration is read.
func (x *gitProxyXfer) git(ctx context.Context, s *gitProxySession, args ...string) (ProxyResult, error) {
	return x.gitWith(ctx, s, gitOpts{}, args...)
}

func (x *gitProxyXfer) gitWith(ctx context.Context, s *gitProxySession, o gitOpts, args ...string) (ProxyResult, error) {
	full := append(append([]string(nil), s.pins...), "--git-dir", x.dir)
	return proxyExec(ctx, ProxyCommand{
		Tool: "git", Path: s.gitPath,
		Args:           append(full, args...),
		Dir:            x.dir,
		Env:            x.env(),
		Stdin:          o.Stdin,
		MaxOutputBytes: o.MaxOutputBytes,
	})
}

func (x *gitProxyXfer) runner(s *gitProxySession) gitRunner {
	return func(ctx context.Context, o gitOpts, args ...string) (ProxyResult, error) {
		return x.gitWith(ctx, s, o, args...)
	}
}

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
		// core.askPass runs a program to obtain a credential, and git consults
		// it BEFORE the terminal — so GIT_TERMINAL_PROMPT=0 does not close it
		// and clearing GIT_ASKPASS/SSH_ASKPASS from the environment only
		// removes the env-var route. Without this pin, a repo-local
		// `core.askPass = ./pwn.sh` executes on the daemon host as the
		// operator during any authenticating fetch or push.
		"core.askPass=",
		"core.gitProxy=",
		"core.editor=false",
		"core.pager=cat",
		"gpg.program=false",
		"diff.external=",
		// Submodule recursion would dial whatever host a submodule's own
		// configuration names — a destination the allow-list never saw. The
		// protocol pins would still apply to that transport; the allow-list
		// would not. The verbs also pass --no-recurse-submodules.
		"fetch.recurseSubmodules=no",
		"push.recurseSubmodules=no",
		"submodule.recurse=false",
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
	// expandTilde for the same reason github_token_file gets it: "~/.ssh/id_ed25519"
	// is how an operator writes this, and ssh -i does not expand it either.
	//
	// Then SINGLE-QUOTE it. core.sshCommand is not an argv — git hands it to a
	// shell when it contains metacharacters, so an operator key path with a
	// space ("~/my keys/id_ed25519") would otherwise split into two arguments
	// and fail with a message pointing nowhere near the cause.
	if key := expandTilde(strings.TrimSpace(policy.SSHKey)); key != "" {
		parts = append(parts, "-i", shellSingleQuote(key), "-o", "IdentitiesOnly=yes")
	}
	return strings.Join(parts, " ")
}

// shellVarHint explains the one path form the daemon deliberately does NOT
// expand. A config file is not a shell, so "${HOME}/token.txt" arrives
// literally — and the resulting "no such file or directory" names a path that
// looks correct, which is a confusing place to be left. "~" IS expanded, so it
// never needs the hint.
func shellVarHint(configured string) string {
	if !strings.Contains(configured, "$") {
		return ""
	}
	return " — note that shell variables like ${HOME} are not expanded in the " +
		"config file; use an absolute path, or \"~/\""
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
		"PATH",   // git needs its helper binaries (git-remote-https, ssh)
		"HOME",   // ~/.gitconfig, ~/.ssh/config — operator-owned
		"TMPDIR", // git writes temporary pack files
		// XDG_CONFIG_HOME must be forwarded for the same reason HOME is: an
		// operator whose git config (and therefore credential.helper) lives
		// under a non-default XDG root would otherwise have it silently
		// dropped, both here and in globalCredentialHelpers.
		"XDG_CONFIG_HOME",
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
// Both probes are bounded by gitProxyProbeTimeout, the same deadline
// gitProbeStrict applies to every other local read. They run on the request
// path, and a `git config` read can block on something outside the daemon's
// control — a stalled network filesystem holding HOME, an unresponsive
// include.path mount — which would otherwise pin the request goroutine here
// until the client gave up.
func globalCredentialHelpers(ctx context.Context, gitPath string) []string {
	probeCtx, cancel := context.WithTimeout(ctx, gitProxyProbeTimeout)
	defer cancel()
	neutral := os.TempDir()
	res, err := proxyExec(probeCtx, ProxyCommand{
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
	res, err = proxyExec(probeCtx, ProxyCommand{
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
func newGitProxySession(ctx context.Context, convID string, remoteScoped bool) (*gitProxySession, *proxyFault) {
	s, fault := newGitProxySessionBase(ctx, remoteScoped)
	if fault != nil {
		return nil, fault
	}
	repoRoot, fault := resolveProxyRepo(ctx, s.gitPath, convID)
	if fault != nil {
		return nil, fault
	}
	s.repoRoot = repoRoot
	return s, nil
}

// newGitProxySessionBase constructs the credential and hardening half of a
// proxy session before its trusted repository root is attached. Normal agent
// proxy calls resolve that root from daemon-recorded launch state above; the
// operator-authenticated worktree picker attaches the repository it already
// resolved from the dashboard request.
func newGitProxySessionBase(ctx context.Context, remoteScoped bool) (*gitProxySession, *proxyFault) {
	cfg, err := config.Load()
	if err != nil {
		return nil, faultf(500, "config", "could not read the daemon configuration: %v", err)
	}
	policy := cfg.ResolvedGitProxy()
	if len(policy.AllowedRemotes) == 0 && !remoteScoped {
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
	pins := gitProxyConfigPins(hooksDir, gitProxySSHCommand(policy),
		globalCredentialHelpers(ctx, gitPath))
	return &gitProxySession{policy: policy, gitPath: gitPath, pins: pins}, nil
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
// PROGRAM rather than describe a destination.
//
// Refusal rather than override is deliberate, and the reason generalises. Three
// separate attempts to neutralize repo-local configuration with `-c` have
// turned out not to work: `uploadpack`/`receivepack` are first-wins across
// scopes so a `-c` override never displaces them, and URL-scoped `http.<url>.*`
// entries beat a generic `-c http.proxy=` by specificity. Which keys a `-c`
// override actually wins is a property of git's config reader, it varies per
// key, and it can change between versions. A repository that sets any of these
// is doing something the proxy has no reason to support, so the honest answer
// is to stop rather than to guess which neutralization still holds.
var dangerousRemoteKeys = []string{"uploadpack", "receivepack", "vcs", "proxy"}

// safeHTTPKeys are the only `http.*` settings a repository may carry. They are
// performance and protocol-version knobs with no security effect.
//
// Everything else in that family is refused, rather than enumerated as a
// deny-list, because the dangerous set is large and open-ended — proxy,
// sslVerify, sslCAInfo, sslCert, curloptResolve (a DNS override!), extraHeader,
// followRedirects, the proxySSL* family — and every one of them can be written
// in the URL-scoped `http.<url>.<key>` form that outranks a generic pin.
var safeHTTPKeys = map[string]bool{
	"http.postbuffer":    true,
	"http.lowspeedlimit": true,
	"http.lowspeedtime":  true,
	"http.maxrequests":   true,
	"http.version":       true,
}

// refuseHostileRepoConfig fails CLOSED when the agent's repository carries
// configuration that could redirect the connection, weaken its TLS, or run a
// program.
//
// It reads the EFFECTIVE configuration rather than `--local`, so a value
// arriving through an `include.path`, through `config.worktree`, or through the
// operator's own global file is caught too. That is the conservative direction:
// an operator who has genuinely set `http.sslVerify` globally gets a clear
// refusal naming the key, rather than a silently weakened connection.
//
// A probe that could not RUN is treated as a refusal, not as "nothing
// configured". That distinction is the whole reason gitProbeStrict exists — a
// gate that reads a timed-out subprocess as "clean" is worse than no gate.
func refuseHostileRepoConfig(ctx context.Context, s *gitProxySession, remoteName string) *proxyFault {
	// One regexp probe for the whole per-remote family, rather than one probe
	// per key: cheaper, and it catches a dangerous key we have not enumerated
	// by surfacing it for the suffix check below.
	remotePattern := "^remote\\." + regexp.QuoteMeta(remoteName) + "\\."
	keys, fault := s.configKeys(ctx, remotePattern)
	if fault != nil {
		return fault
	}
	for _, key := range keys {
		suffix := key[strings.LastIndexByte(key, '.')+1:]
		for _, dangerous := range dangerousRemoteKeys {
			if suffix == dangerous {
				return faultf(http.StatusForbidden, "remote_config_refused",
					"this repository sets %s, which selects a program rather than a destination; "+
						"the proxy refuses to run against a remote configured that way", key)
			}
		}
	}

	httpKeys, fault := s.configKeys(ctx, "^http\\.")
	if fault != nil {
		return fault
	}
	for _, key := range httpKeys {
		if safeHTTPKeys[key] {
			continue
		}
		return faultf(http.StatusForbidden, "http_config_refused",
			"this repository configures %s; that family can redirect the connection or disable "+
				"certificate verification — and in its URL-scoped form it outranks any override the "+
				"proxy could set — so the proxy refuses to run against it", key)
	}

	// credential.* names a PROGRAM git runs during authentication, in both the
	// generic `credential.helper` form and the URL-scoped
	// `credential.<url>.helper` form. The pins do reset the whole helper list
	// (verified on git 2.43: a `-c credential.helper=` read last clears
	// URL-scoped entries too), but that is a precedence property of git's
	// config reader — the same kind of assumption that turned out to be wrong
	// for remote.<n>.uploadpack and for http.<url>.*. So the repo-controlled
	// scopes are refused outright rather than relied upon to be overridden.
	//
	// Only local and worktree are refused. global and system are the
	// OPERATOR's, and globalCredentialHelpers deliberately re-applies their
	// helpers; command scope is the proxy's own pins.
	credentialKeys, fault := s.configKeysInScopes(ctx, "^credential\\.", "local", "worktree")
	if fault != nil {
		return fault
	}
	if len(credentialKeys) > 0 {
		return faultf(http.StatusForbidden, "credential_config_refused",
			"this repository configures %s; credential.* selects the program git runs to obtain a "+
				"credential, so the proxy refuses to run against a repository that sets it. "+
				"Move the setting to your global or system git configuration if it is yours",
			credentialKeys[0])
	}
	return nil
}

// gitProxyCommandScope is git's name for a value that arrived as a `-c`
// override — which, for any command this proxy runs, means the proxy put it
// there. The agent has no way to add one.
const gitProxyCommandScope = "command"

// configKeys lists the configured keys matching a git config regexp, EXCLUDING
// the proxy's own `-c` overrides.
//
// That exclusion is load-bearing, not tidiness. gitProxyConfigPins passes
// `-c http.proxy=` on every single invocation, and `git config --get-regexp`
// reports command-scope values exactly like any other — so the http gate below
// read the proxy's OWN pin, named it as hostile repository configuration, and
// refused. Every request, for every repository. That is what happened the first
// time this feature was pointed at a real repo, and no stub-backed test could
// have caught it: the stub answers from a fixture map and never sees the pins
// the daemon actually passes.
//
// Excluding by deny-list rather than allow-listing the four real scopes is
// deliberate. An unrecognised scope must keep counting as "configured
// somewhere", so a git that grows a new one fails CLOSED.
func (s *gitProxySession) configKeys(ctx context.Context, pattern string) ([]string, *proxyFault) {
	scoped, fault := s.configKeysScoped(ctx, pattern)
	if fault != nil {
		return nil, fault
	}
	var keys []string
	for _, entry := range scoped {
		if entry.scope != gitProxyCommandScope {
			keys = append(keys, entry.key)
		}
	}
	return keys, nil
}

// configKeysInScopes lists the configured keys matching a git config regexp
// that come from one of the named scopes ("local", "worktree", "global",
// "system", "command").
func (s *gitProxySession) configKeysInScopes(ctx context.Context, pattern string, scopes ...string) ([]string, *proxyFault) {
	scoped, fault := s.configKeysScoped(ctx, pattern)
	if fault != nil {
		return nil, fault
	}
	wanted := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		wanted[s] = true
	}
	var keys []string
	for _, entry := range scoped {
		if wanted[entry.scope] {
			keys = append(keys, entry.key)
		}
	}
	return keys, nil
}

// scopedConfigKey is one `--show-scope` row.
type scopedConfigKey struct{ scope, key string }

// configKeysScoped runs the one probe both callers above filter.
//
// --show-scope rather than --local is what makes this trustworthy: `git config
// --local --get-regexp` does NOT report a key that reached the local file
// through include.path, while --show-scope reports it and attributes it to the
// including scope. Filtering the effective listing is therefore strictly more
// complete than reading one scope's file.
//
// It returns no error for "no matches" (git's exit 1) and a refusal for
// anything else, including a probe that never ran.
func (s *gitProxySession) configKeysScoped(ctx context.Context, pattern string) ([]scopedConfigKey, *proxyFault) {
	out, exitCode, ran := s.gitProbeStrict(ctx,
		"config", "--show-scope", "--name-only", "--get-regexp", "--", pattern)
	if !ran {
		return nil, faultf(http.StatusInternalServerError, "config_probe_failed",
			"could not inspect this repository's configuration for %s; refusing rather than "+
				"assuming it is safe", pattern)
	}
	if exitCode == 1 {
		return nil, nil // git's "no matching key"
	}
	if exitCode != 0 {
		return nil, faultf(http.StatusInternalServerError, "config_probe_failed",
			"inspecting this repository's configuration for %s failed (git exit %d); refusing "+
				"rather than assuming it is safe", pattern, exitCode)
	}
	var scoped []scopedConfigKey
	for _, line := range splitNonEmptyLines(out) {
		// "<scope>\t<key>". A key with no scope column would be a git that does
		// not understand --show-scope; the exit checks above have already ruled
		// that out, so treat the whole line as the key with an unknown scope
		// rather than dropping it — an unclassified key must still count as
		// configured.
		scope, key, ok := strings.Cut(line, "\t")
		if !ok {
			scoped = append(scoped, scopedConfigKey{scope: "", key: strings.TrimSpace(line)})
			continue
		}
		scoped = append(scoped, scopedConfigKey{
			scope: strings.TrimSpace(scope),
			key:   strings.TrimSpace(key),
		})
	}
	return scoped, nil
}

// gitOpts carries the per-call overrides on a hardened git invocation. Both
// fields exist for the ref transfer: a whole update-ref transaction arrives on
// stdin, and a ref listing needs a bigger output bound than a diagnosis does.
type gitOpts struct {
	Stdin          string
	MaxOutputBytes int
}

// gitRunner is "a hardened git invocation, somewhere" — either in the agent's
// repository or in the transfer directory. The ref helpers below work against
// both, and which one they are given is exactly what decides whether a command
// reads agent-controlled configuration.
type gitRunner func(ctx context.Context, o gitOpts, args ...string) (ProxyResult, error)

// git runs a hardened git command in the agent's repository.
func (s *gitProxySession) git(ctx context.Context, args ...string) (ProxyResult, error) {
	return s.gitWith(ctx, gitOpts{}, args...)
}

func (s *gitProxySession) gitWith(ctx context.Context, o gitOpts, args ...string) (ProxyResult, error) {
	return proxyExec(ctx, ProxyCommand{
		Tool:           "git",
		Path:           s.gitPath,
		Args:           append(append([]string(nil), s.pins...), args...),
		Dir:            s.repoRoot,
		Env:            gitProxyEnv(),
		Stdin:          o.Stdin,
		MaxOutputBytes: o.MaxOutputBytes,
	})
}

func (s *gitProxySession) runner() gitRunner { return s.gitWith }

// gitProbe runs a local git command and returns its trimmed stdout, treating a
// non-zero exit as "no answer" rather than an error. Probes are questions about
// the repository ("what is the current branch?"), and an unanswerable question
// is handled by the caller, not surfaced as a subprocess failure.
//
// Use this only where "" is a SAFE answer. A security gate must not: for those,
// use gitProbeStrict, which distinguishes "git said no" from "the probe did not
// run". See refuseHostileRepoConfig.
func (s *gitProxySession) gitProbe(ctx context.Context, args ...string) string {
	value, _, _ := s.gitProbeStrict(ctx, args...)
	return value
}

// gitProbeStrict is gitProbe with the outcome split three ways, because a gate
// that reads a failed probe as "nothing configured" fails OPEN.
//
//	value          trimmed stdout (empty unless exit == 0)
//	exitCode       git's exit status; 1 conventionally means "not found"
//	ran            false when the subprocess could not be executed at all
//	               (spawn failure, timeout) — never a statement about content
func (s *gitProxySession) gitProbeStrict(ctx context.Context, args ...string) (value string, exitCode int, ran bool) {
	probeCtx, cancel := context.WithTimeout(ctx, gitProxyProbeTimeout)
	defer cancel()
	res, err := s.git(probeCtx, args...)
	if err != nil || res.TimedOut {
		return "", -1, false
	}
	if res.ExitCode != 0 {
		return "", res.ExitCode, true
	}
	return strings.TrimSpace(res.Stdout), 0, true
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
	return resolveProxyRepoAt(ctx, gitPath, convID, "")
}

// resolveProxyRepoAt is resolveProxyRepo with an optional repository directory
// selected by the caller. The selection is admitted only inside the caller's
// immutable launch tree; this supports agents launched at a workspace root
// containing several repositories without turning the path into an aiming
// primitive.
func resolveProxyRepoAt(ctx context.Context, gitPath, convID, requestedDir string) (string, *proxyFault) {
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
	if strings.TrimSpace(requestedDir) != "" {
		launchDir = strings.TrimSpace(requestedDir)
		if !callerOwnedDirTrust(convID, launchDir) {
			return "", faultf(403, "repo_outside_launch_dir",
				"the repository directory must be the calling agent's launch directory or a subdirectory of it")
		}
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
	root = filepath.Clean(root)

	// The work tree is not the whole story. A `.git` GITFILE ("gitdir: …")
	// leaves the toplevel pointing at the agent's own directory while the
	// actual GIT_DIR — config, refs, objects and therefore remotes — lives
	// somewhere else entirely. Without this check an agent could drop a
	// one-line .git file naming another repository's admin dir and have the
	// daemon list, fetch into, and push from a repo that is not its own.
	//
	// Linked worktrees legitimately use a gitfile too, and theirs points at
	// <common>/worktrees/<name>, which is not under the work tree either. They
	// are admitted by acceptLinkedWorktree below, which proves the link is real
	// rather than assuming it — see there for why the shape alone is not
	// enough.
	gitDirRes, err := proxyExec(probeCtx, ProxyCommand{
		Tool: "git",
		Path: gitPath,
		Args: []string{"-C", resolved, "rev-parse", "--absolute-git-dir"},
		Dir:  resolved,
		Env:  gitProxyEnv(),
	})
	if err != nil {
		return "", faultf(500, "repo_unresolved", "could not inspect the repository: %v", err)
	}
	gitDir := strings.TrimSpace(gitDirRes.Stdout)
	if gitDirRes.ExitCode != 0 || gitDir == "" {
		return "", faultf(409, "not_a_repo",
			"could not resolve the git directory for %s", resolved)
	}
	if resolvedGitDir, err := filepath.EvalSymlinks(gitDir); err == nil {
		gitDir = resolvedGitDir
	}
	gitDir = filepath.Clean(gitDir)
	if !sandboxpolicy.PathContainsOrEqual(root, gitDir) {
		if fault := acceptLinkedWorktree(ctx, gitPath, resolved, root, gitDir); fault != nil {
			return "", fault
		}
	}

	// An agent launched somewhere that is not itself a repository makes git
	// walk upward, and if the operator's home happens to be a dotfiles repo the
	// daemon would end up operating on THAT. Refuse the shape rather than the
	// symptom: a work tree at or above home is never the agent's own project.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if resolvedHome, err := filepath.EvalSymlinks(home); err == nil {
			home = resolvedHome
		}
		if sandboxpolicy.PathContainsOrEqual(root, filepath.Clean(home)) {
			return "", faultf(409, "repo_too_broad",
				"the git work tree containing your launch directory is %s, which contains the "+
					"operator's home directory; refusing to treat that as your project repository",
				root)
		}
	}
	return root, nil
}

// gitDirRedirected is the refusal for a git directory the proxy will not
// attribute to the caller. Shared so the plain-redirect and failed-worktree
// paths read identically to an agent: both mean "this is not your repository's
// own metadata".
func gitDirRedirected(gitDir, root, because string) *proxyFault {
	return faultf(409, "git_dir_redirected",
		"this work tree's git directory (%s) lives outside the work tree (%s)%s; "+
			"the proxy only operates on a repository whose own metadata it can attribute to you",
		gitDir, root, because)
}

// acceptLinkedWorktree decides whether a git directory sitting OUTSIDE the work
// tree is a genuine linked worktree of its own main repository, rather than a
// hand-written `.git` gitfile aimed at somebody else's.
//
// This case has to be admitted: `tclaude worktree` creates linked worktrees,
// and that is the layout a tclaude agent normally runs in. Refusing it made the
// proxy unusable for its main audience.
//
// It must not be admitted on SHAPE alone, though. Git does not validate the
// link, and the two are indistinguishable from the work tree's side: writing
//
//	gitdir: /victim/.git/worktrees/wt1
//
// into a `.git` file yields a work tree whose toplevel is the attacker's own
// directory and whose remotes, objects and config are the victim's. Verified
// against git 2.43 — every rev-parse answer comes back looking legitimate, and
// `git remote get-url origin` returns the victim's remote.
//
// What an attacker cannot forge is the BACK-POINTER. A real linked worktree is
// registered in its main repository: `<common>/worktrees/<name>/gitdir` holds
// the path of that work tree's own `.git` file. A forged link necessarily
// points at an entry whose back-pointer names a DIFFERENT work tree, so
// requiring the two to agree is what separates the cases. Writing a matching
// back-pointer needs write access to the main repository's admin directory, and
// an agent with that can already rewrite the repository's config — so honouring
// the link grants no reach it did not already have. That is the invariant this
// whole feature rests on: credentials, never reach.
func acceptLinkedWorktree(ctx context.Context, gitPath, dir, root, gitDir string) *proxyFault {
	// Bounded like every other local probe. This one runs on the request path,
	// and a work tree on a stalled filesystem would otherwise pin the request
	// goroutine here with no deadline of its own.
	probeCtx, cancel := context.WithTimeout(ctx, gitProxyProbeTimeout)
	defer cancel()
	commonRes, err := proxyExec(probeCtx, ProxyCommand{
		Tool: "git",
		Path: gitPath,
		Args: []string{"-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir"},
		Dir:  dir,
		Env:  gitProxyEnv(),
	})
	if err != nil || commonRes.ExitCode != 0 {
		return gitDirRedirected(gitDir, root, "")
	}
	// Canonicalise, do not merely Clean. The caller resolved gitDir through
	// EvalSymlinks, so comparing it against a commonDir that still contains a
	// symlinked component compares two different spellings of the same place —
	// and a work tree reached through, say, a symlinked /home would be refused
	// as redirected. Both sides go through the same resolution.
	commonDir := canonicalProxyPath(strings.TrimSpace(commonRes.Stdout))
	if !filepath.IsAbs(commonDir) || commonDir == gitDir {
		return gitDirRedirected(gitDir, root, "")
	}
	// Structure: a linked worktree's git dir is always <common>/worktrees/<name>.
	if canonicalProxyPath(filepath.Join(commonDir, "worktrees", filepath.Base(gitDir))) != gitDir {
		return gitDirRedirected(gitDir, root, "")
	}
	// The back-pointer, which is the part that actually proves anything.
	registered, err := os.ReadFile(filepath.Join(gitDir, "gitdir"))
	if err != nil {
		return gitDirRedirected(gitDir, root,
			" and is not registered as a linked worktree of that repository")
	}
	if canonicalProxyPath(strings.TrimSpace(string(registered))) !=
		canonicalProxyPath(filepath.Join(root, ".git")) {
		return gitDirRedirected(gitDir, root,
			" and that repository has it registered against a different work tree")
	}
	return nil
}

// canonicalProxyPath resolves a path as far as the filesystem allows, so two
// spellings of the same location compare equal. A path that cannot be resolved
// (it may not exist) is merely cleaned — the caller is comparing for equality,
// not deciding reachability.
func canonicalProxyPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(p)
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

// contacted names the host/owner/repo a verb actually dials. It is not always
// FetchRef: remote.<name>.pushurl may point somewhere else entirely (both are
// validated and allow-listed), and an audit row that recorded the fetch host
// for a push would name a destination the push never touched.
func (r resolvedRemote) contacted(push bool) remoteRef {
	if push {
		return r.PushRef
	}
	return r.FetchRef
}

// resolveProxyRemote validates a remote name end to end.
//
// Both the fetch URL and the PUSH url are validated: remote.<name>.pushurl can
// differ from remote.<name>.url, so validating only the latter would leave
// push aimed somewhere unchecked.
//
// What defeats url.<base>.insteadOf — the one dangerous key a `-c` override
// cannot reset — is the ALLOW-LIST, not the fixed-point check below. `git
// remote get-url` already returns the REWRITTEN url (verified on git 2.43), so
// every URL reaching parseRemoteURL and remoteAllowed here is the destination
// git would really dial. A repository that rewrites github.com to
// attacker.example is refused because attacker.example is not allow-listed.
//
// The fixed-point check (`ls-remote --get-url <url>` returns the same string)
// is kept as defence-in-depth against a git that stops applying rewrites in
// `remote get-url`, which would silently move the real destination out from
// under the allow-list. On current git it never fires; that is expected, and it
// is why the allow-list is what the tests pin.
func resolveProxyRemote(ctx context.Context, s *gitProxySession, name string) (resolvedRemote, *proxyFault) {
	if fault := validateRemoteName(name); fault != nil {
		return resolvedRemote{}, fault
	}
	if fault := refuseHostileRepoConfig(ctx, s, name); fault != nil {
		return resolvedRemote{}, fault
	}
	// --all, not the bare form. A remote may carry SEVERAL url / pushurl
	// values, and `git push <name>` contacts EVERY one of them — while `git
	// remote get-url <name>` reports only the first. Validating just the first
	// would let a repository keep an allow-listed URL in position one and
	// append an arbitrary second host that every gate here never sees.
	fetchURLs, fault := s.remoteURLs(ctx, name, false)
	if fault != nil {
		return resolvedRemote{}, fault
	}
	if len(fetchURLs) == 0 {
		return resolvedRemote{}, faultf(404, "unknown_remote",
			"no remote named %q is configured in %s", name, filepath.Base(s.repoRoot))
	}
	pushURLs, fault := s.remoteURLs(ctx, name, true)
	if fault != nil {
		return resolvedRemote{}, fault
	}
	if len(pushURLs) == 0 {
		// Defensive only. `git remote get-url --push --all` reports the FETCH
		// urls when no remote.<name>.pushurl is configured (verified on git
		// 2.43), so this is not the "no pushurl" path — that answer already
		// arrived above. It covers a git that answers with nothing at all,
		// where treating push as going wherever fetch goes is the honest read.
		pushURLs = fetchURLs
	}

	out := resolvedRemote{
		Name:     name,
		FetchURL: fetchURLs[0],
		PushURL:  pushURLs[0],
	}
	for _, check := range []struct {
		label string
		urls  []string
	}{
		{"fetch", fetchURLs},
		{"push", pushURLs},
	} {
		for _, raw := range check.urls {
			ref, err := parseRemoteURL(raw)
			if err != nil {
				return resolvedRemote{}, faultf(403, "remote_refused",
					"remote %q %s URL: %v", name, check.label, err)
			}
			if len(s.policy.AllowedRemotes) > 0 && !remoteAllowed(ref, s.policy.AllowedRemotes) {
				return resolvedRemote{}, faultf(403, "remote_not_allowed",
					"remote %q (%s %s) is not on the operator's allow-list; allowed: %s",
					name, check.label, ref.Key(), strings.Join(s.policy.AllowedRemotes, ", "))
			}
			if rewritten := s.gitProbe(ctx, "ls-remote", "--get-url", "--", raw); rewritten != raw {
				return resolvedRemote{}, faultf(403, "remote_rewritten",
					"this repository rewrites its %s URL (url.*.insteadOf); refusing to follow a redirect that was not validated",
					check.label)
			}
			if check.label == "fetch" && raw == out.FetchURL {
				out.FetchRef = ref
			}
			if check.label == "push" && raw == out.PushURL {
				out.PushRef = ref
			}
		}
	}
	return out, nil
}

// remoteURLs lists every URL configured for a remote, in git's own order.
func (s *gitProxySession) remoteURLs(ctx context.Context, name string, push bool) ([]string, *proxyFault) {
	args := []string{"remote", "get-url", "--all"}
	if push {
		args = append(args, "--push")
	}
	args = append(args, "--", name)
	out, exitCode, ran := s.gitProbeStrict(ctx, args...)
	if !ran {
		return nil, faultf(http.StatusInternalServerError, "config_probe_failed",
			"could not read the URLs configured for remote %q; refusing rather than guessing", name)
	}
	if exitCode != 0 {
		// "No such remote" — git exits 2. Reported as "nothing here" so the
		// caller can phrase its own 404; note that a remote with no
		// remote.<name>.pushurl does NOT land here, because `--push` falls back
		// to the fetch URLs and exits 0.
		return nil, nil
	}
	return splitNonEmptyLines(out), nil
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
