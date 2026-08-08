package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// gitproxy_handlers.go holds the HTTP surface of `tclaude proxy git`. The
// hardening lives in gitproxy.go; this file is the request/response shape and
// the order of the gates.
//
// Every gate runs in the same order, and the order matters:
//
//  1. permission preflight — an unscoped grant is decided immediately; a
//     scoped grant is deferred until the normalized remote is known;
//  2. legacy operator-global policy, when configured;
//  3. the repo gate — daemon-recorded launch directory, no request parameter;
//  4. parameter validation — charset, length, no leading "-";
//  5. the remote gate — URL parse, allow-list, insteadOf fixed point;
//  6. the scoped permission decision against that exact normalized remote;
//  7. only then, a credentialed/network subprocess.
//
// Why the network reads are POST
//
// `ls-remote` and `fetch` are conceptually reads, but they SPEND THE
// OPERATOR'S CREDENTIAL against a remote host. The audit middleware records
// mutating methods only, and a credentialed outbound call is exactly the thing
// an operator should be able to review after the fact — so they are POSTs.
// `remotes` stays a GET: it is a purely local inspection that touches no
// network and uses no credential.

// maxGitProxyRequestBytes bounds a proxy request body before decoding. These
// bodies are a handful of short scalars; nothing legitimate approaches this.
const maxGitProxyRequestBytes = 16 * 1024

// gitProxyOutcome is the common tail of every proxied git response.
//
// The daemon returns HTTP 200 whenever it SUCCESSFULLY RAN the command, even
// when git itself failed: a non-fast-forward push is a real answer that the
// agent must read and act on, not a daemon error. ExitCode plus Stderr carry
// git's verdict verbatim, and the CLI exits non-zero on a non-zero ExitCode.
// A non-2xx from these routes therefore means the daemon refused or could not
// run the command at all.
type gitProxyOutcome struct {
	Repo      string `json:"repo"`
	Remote    string `json:"remote,omitempty"`
	RemoteRef string `json:"remote_ref,omitempty"`
	Branch    string `json:"branch,omitempty"`
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	TimedOut  bool   `json:"timed_out,omitempty"`
}

type gitProxyRemoteView struct {
	Name       string `json:"name"`
	FetchURL   string `json:"fetch_url,omitempty"`
	PushURL    string `json:"push_url,omitempty"`
	RemoteRef  string `json:"remote_ref,omitempty"`
	Allowed    bool   `json:"allowed"`
	RefusedFor string `json:"refused_for,omitempty"`
}

type gitProxyRemotesResponse struct {
	// RepoPath is the FULL work-tree path, deliberately unlike gitProxyOutcome's
	// `repo` (a basename) and ghProxyOutcome's `repo` (an owner/repo slug).
	// Discovery is the one place the agent needs to see exactly which checkout
	// the daemon resolved, so it gets a distinct name rather than a third
	// meaning for the same one.
	RepoPath       string               `json:"repo_path"`
	Branch         string               `json:"branch,omitempty"`
	Remotes        []gitProxyRemoteView `json:"remotes"`
	AllowedRemotes []string             `json:"allowed_remotes"`
	ProtectedRefs  []string             `json:"protected_refs"`
	AllowForcePush bool                 `json:"allow_force_push"`
}

type gitProxyFetchRequest struct {
	Remote string `json:"remote,omitempty"`
	Branch string `json:"branch,omitempty"`
	Prune  bool   `json:"prune,omitempty"`
	Tags   bool   `json:"tags,omitempty"`
}

type gitProxyPushRequest struct {
	Remote         string `json:"remote,omitempty"`
	Branch         string `json:"branch,omitempty"`
	SetUpstream    bool   `json:"set_upstream,omitempty"`
	ForceWithLease bool   `json:"force_with_lease,omitempty"`
}

type gitProxyLsRemoteRequest struct {
	Remote  string `json:"remote,omitempty"`
	Heads   bool   `json:"heads,omitempty"`
	Tags    bool   `json:"tags,omitempty"`
	Pattern string `json:"pattern,omitempty"`
}

// defaultProxyRemote is used when a request names none. "origin" is the
// near-universal convention and keeping the default fixed (rather than
// inferring it from repo-local branch configuration) keeps the resolution
// predictable and auditable.
const defaultProxyRemote = "origin"

func remoteOrDefault(name string) string {
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	return defaultProxyRemote
}

// decodeGitProxyBody reads a bounded JSON body. An absent body is fine — every
// field of every proxy request has a usable default.
func decodeGitProxyBody(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxGitProxyRequestBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return false
	}
	return true
}

// preflightProxyPermission preserves the cheap refusal for ungranted callers
// while allowing a scoped grant to wait for the local remote resolution that
// gives it meaning. The caller MUST finish a deferred decision with
// requirePermission and ActionContext{Remote: ...} before any network touch or
// side effect.
func preflightProxyPermission(w http.ResponseWriter, r *http.Request, perm string) (convID string, deferred, ok bool) {
	p := peerFromContext(r.Context())
	if classify(p) == classAgent {
		v := resolvePermissionVerdictForRequest(r, p.ConvID, perm)
		if v.Resolution == permAllow && !evalPermissionScope(v, p.ConvID, ActionContext{}).Unscoped {
			state, err := db.AgentState(p.ConvID)
			if err != nil {
				writeError(w, http.StatusForbidden, "auth", "could not verify caller agent state")
				return "", false, false
			}
			if state == db.AgentStateRetired {
				writeError(w, http.StatusForbidden, "auth", "caller is a retired agent")
				return "", false, false
			}
			return p.ConvID, true, true
		}
	}
	convID, ok = requirePermission(w, r, perm)
	return convID, false, ok
}

func finishProxyPermission(w http.ResponseWriter, r *http.Request, convID, perm, remote string, deferred bool) bool {
	if !deferred {
		return true
	}
	authorizedConvID, ok := requirePermission(w, r, perm, ActionContext{Remote: remote})
	return ok && authorizedConvID == convID
}

// handleGitProxyRemotes serves GET /v1/git/remotes — the "what can I reach
// from here" question an agent should ask before anything else.
//
// It is the one proxy route that touches no network, so it is deliberately
// informative about REFUSALS too: a remote that fails the allow-list is listed
// with the reason, so the agent can tell its human exactly what to add rather
// than guessing from a 403 on a later push.
func handleGitProxyRemotes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	convID, deferred, ok := preflightProxyPermission(w, r, PermGitRead)
	if !ok {
		return
	}
	ctx := r.Context()
	s, fault := newGitProxySession(ctx, convID, deferred)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	resp := gitProxyRemotesResponse{
		RepoPath:       s.repoRoot,
		Branch:         s.currentBranch(ctx),
		Remotes:        []gitProxyRemoteView{},
		AllowedRemotes: s.policy.AllowedRemotes,
		AllowForcePush: s.policy.AllowForcePush,
		ProtectedRefs:  []string{},
	}
	if s.policy.ProtectedRefs != nil {
		resp.ProtectedRefs = *s.policy.ProtectedRefs
	}
	var scopedVerdict *permVerdict
	if deferred {
		v := resolvePermissionVerdictForRequest(r, convID, PermGitRead)
		scopedVerdict = &v
	}
	for i, name := range splitNonEmptyLines(s.gitProbe(ctx, "remote")) {
		if i >= maxGitProxyRemotes {
			break
		}
		resp.Remotes = append(resp.Remotes, describeProxyRemote(ctx, s, name, convID, scopedVerdict))
	}
	writeJSON(w, http.StatusOK, resp)
}

// describeProxyRemote answers "could I use this remote?" for the listing.
//
// It deliberately runs the CHEAP half of the gate — the URLs and the allow-list
// — rather than the full resolveProxyRemote. This route is an unauthenticated-
// to-the-network local read that an agent may call freely, and running every
// gate for every remote would turn one GET into hundreds of subprocesses. The
// expensive checks (the config-family probes, the insteadOf fixed point) still
// run on the operation itself, which is the only place their answer matters.
//
// The consequence is honest and worth stating: a remote shown as allowed here
// can still be refused by an actual fetch or push, and the refusal will say
// why. The listing answers "is this remote on the allow-list", not "is this
// repository in a state the proxy will act on".
func describeProxyRemote(ctx context.Context, s *gitProxySession, name, convID string, scoped *permVerdict) gitProxyRemoteView {
	view := gitProxyRemoteView{Name: name}
	if validateRemoteName(name) != nil {
		// A remote whose own name we would refuse is reported rather than
		// hidden: the agent needs to know why it cannot use it.
		view.RefusedFor = "the remote name is outside the accepted charset"
		return view
	}
	fetchURLs, fault := s.remoteURLs(ctx, name, false)
	if fault != nil {
		view.RefusedFor = fault.Msg
		return view
	}
	if len(fetchURLs) == 0 {
		view.RefusedFor = "no URL is configured for this remote"
		return view
	}
	pushURLs, _ := s.remoteURLs(ctx, name, true)
	if len(pushURLs) == 0 {
		pushURLs = fetchURLs
	}
	view.FetchURL = strings.Join(fetchURLs, ", ")
	view.PushURL = strings.Join(pushURLs, ", ")

	// Every configured URL must pass, not just the first — that asymmetry is
	// exactly what a second, unlisted url line would exploit.
	for _, raw := range append(append([]string{}, fetchURLs...), pushURLs...) {
		ref, err := parseRemoteURL(raw)
		if err != nil {
			view.RefusedFor = err.Error()
			return view
		}
		if !remoteAllowed(ref, s.policy.AllowedRemotes) {
			view.RefusedFor = fmt.Sprintf(
				"%s is not on the operator's allow-list (allowed: %s)",
				ref.Key(), strings.Join(s.policy.AllowedRemotes, ", "))
			return view
		}
		if view.RemoteRef == "" {
			view.RemoteRef = ref.Key()
		}
	}
	if scoped != nil {
		if !evalPermissionScope(*scoped, convID, ActionContext{Remote: view.RemoteRef}).Satisfied {
			view.RefusedFor = fmt.Sprintf("%s is outside this caller's git.read remote scope", view.RemoteRef)
			return view
		}
	}
	view.Allowed = true
	return view
}

// handleGitProxyLsRemote serves POST /v1/git/ls-remote — the cheapest
// credentialed round trip, and the one an agent should use to check whether a
// branch already exists on the remote.
func handleGitProxyLsRemote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	convID, deferred, ok := preflightProxyPermission(w, r, PermGitRead)
	if !ok {
		return
	}
	var body gitProxyLsRemoteRequest
	if !decodeGitProxyBody(w, r, &body) {
		return
	}
	ctx := r.Context()
	s, resolved, fault := openProxyRemote(ctx, convID, body.Remote, deferred)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if !finishProxyPermission(w, r, convID, PermGitRead, resolved.FetchRef.Key(), deferred) {
		return
	}
	args := []string{"ls-remote", gitProxyUploadPack}
	if body.Heads {
		args = append(args, "--heads")
	}
	if body.Tags {
		args = append(args, "--tags")
	}
	// The validated URL, not the remote name — same reason as push: resolving a
	// name means reading the agent's config with the credential in hand.
	args = append(args, "--", resolved.FetchURL)
	if pattern := strings.TrimSpace(body.Pattern); pattern != "" {
		if fault := validateRefPattern(pattern); fault != nil {
			writeProxyFault(w, fault)
			return
		}
		args = append(args, pattern)
	}
	s.runIsolatedAndRespond(ctx, w, r, resolved, "", args, false)
}

// handleGitProxyFetch serves POST /v1/git/fetch. Fetch never updates the
// working tree, which is what keeps .gitattributes filter programs out of the
// daemon's blast radius — see the gitproxy.go header.
func handleGitProxyFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	convID, deferred, ok := preflightProxyPermission(w, r, PermGitRead)
	if !ok {
		return
	}
	var body gitProxyFetchRequest
	if !decodeGitProxyBody(w, r, &body) {
		return
	}
	ctx := r.Context()
	s, resolved, fault := openProxyRemote(ctx, convID, body.Remote, deferred)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if !finishProxyPermission(w, r, convID, PermGitRead, resolved.FetchRef.Key(), deferred) {
		return
	}
	args := []string{"fetch", gitProxyUploadPack, "--no-recurse-submodules"}
	if body.Prune {
		args = append(args, "--prune")
	}
	if body.Tags {
		args = append(args, "--tags")
	}
	args = append(args, "--", resolved.Name)
	branch := strings.TrimSpace(body.Branch)
	if branch != "" {
		if fault := validateBranchName(branch); fault != nil {
			writeProxyFault(w, fault)
			return
		}
		// An explicit, fully-qualified refspec rather than a bare branch name:
		// it says exactly what will be written locally, and it cannot be
		// reinterpreted by the remote's own refspec configuration.
		args = append(args, fmt.Sprintf("refs/heads/%s:refs/remotes/%s/%s", branch, resolved.Name, branch))
	}
	s.runAndRespond(ctx, w, r, resolved, branch, args)
}

// handleGitProxyPush serves POST /v1/git/push — the only route that writes to
// a remote, and the only one behind the separate git.push slug.
func handleGitProxyPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	convID, deferred, ok := preflightProxyPermission(w, r, PermGitPush)
	if !ok {
		return
	}
	var body gitProxyPushRequest
	if !decodeGitProxyBody(w, r, &body) {
		return
	}
	ctx := r.Context()
	s, resolved, fault := openProxyRemote(ctx, convID, body.Remote, deferred)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	if !finishProxyPermission(w, r, convID, PermGitPush, resolved.PushRef.Key(), deferred) {
		return
	}

	branch := strings.TrimSpace(body.Branch)
	if branch == "" {
		branch = s.currentBranch(ctx)
		if branch == "" {
			writeError(w, http.StatusConflict, "detached_head",
				"this work tree is not on a branch (detached HEAD); check out a branch or name one explicitly")
			return
		}
	}
	if fault := validateBranchName(branch); fault != nil {
		writeProxyFault(w, fault)
		return
	}

	protected := []string{}
	if s.policy.ProtectedRefs != nil {
		protected = *s.policy.ProtectedRefs
	}
	if refProtected(branch, protected) {
		writeError(w, http.StatusForbidden, "protected_ref", fmt.Sprintf(
			"%q is an operator-protected branch; the proxy will not push to it. "+
				"Push a feature branch and open a pull request instead. "+
				"(Protected: %s — agent.git_proxy.protected_refs.)",
			branch, strings.Join(protected, ", ")))
		return
	}

	// The credentialed push runs from a DAEMON-OWNED git directory, not the
	// agent's repository — see gitProxyXfer. Everything below therefore has to
	// be resolved from the agent's repo FIRST, and then named explicitly, since
	// the pushing command can no longer look anything up for itself.
	sha, exit, ran := s.gitProbeStrict(ctx, "rev-parse", "--verify", "refs/heads/"+branch)
	if !ran || exit != 0 || sha == "" {
		writeError(w, http.StatusConflict, "unknown_branch",
			fmt.Sprintf("branch %q does not exist in this repository", branch))
		return
	}

	args := []string{"push", gitProxyReceivePack, "--no-recurse-submodules"}
	if body.ForceWithLease {
		if !s.policy.AllowForcePush {
			writeError(w, http.StatusForbidden, "force_push_disabled",
				"force-pushing is disabled; the operator can enable it with "+
					"agent.git_proxy.allow_force_push")
			return
		}
		// A bare --force-with-lease compares against the remote-tracking ref of
		// the repository it runs in, and the transfer directory has none — it
		// would degrade to no protection at all. So the expected value is read
		// from the agent's own remote-tracking ref and stated explicitly, which
		// is both safe here and more precise than the bare form.
		lease, exit, ran := s.gitProbeStrict(ctx, "rev-parse", "--verify",
			"refs/remotes/"+resolved.Name+"/"+branch)
		if !ran || exit != 0 || lease == "" {
			writeError(w, http.StatusConflict, "no_lease_base",
				fmt.Sprintf("cannot force-with-lease: this repository has no "+
					"refs/remotes/%s/%s to lease against. Fetch first, so there is a known "+
					"remote state to refuse to overwrite.", resolved.Name, branch))
			return
		}
		args = append(args, "--force-with-lease=refs/heads/"+branch+":"+lease)
	}

	// The validated URL, spelled out. Not the remote NAME: a name means the
	// pushing command has to read remote.<name>.url from somewhere, and the
	// only place holding it is the file this design exists to stop consulting.
	args = append(args, "--", resolved.PushURL,
		fmt.Sprintf("%s:refs/heads/%s", sha, branch))

	s.runIsolatedAndRespond(ctx, w, r, resolved, branch, args, body.SetUpstream)
}

// openProxyRemote runs gates 2, 3 and 5 for the routes that name a remote.
func openProxyRemote(ctx context.Context, convID, requestedRemote string, remoteScoped bool) (
	*gitProxySession, resolvedRemote, *proxyFault,
) {
	name := remoteOrDefault(requestedRemote)
	s, fault := newGitProxySession(ctx, convID, remoteScoped)
	if fault != nil {
		return nil, resolvedRemote{}, fault
	}
	resolved, fault := resolveProxyRemote(ctx, s, name)
	if fault != nil {
		return nil, resolvedRemote{}, fault
	}
	return s, resolved, nil
}

// currentBranch reports the branch the work tree is on, or "" when detached.
func (s *gitProxySession) currentBranch(ctx context.Context) string {
	branch := s.gitProbe(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "HEAD" {
		return ""
	}
	return branch
}

// runAndRespond executes a network git command under the network timeout,
// records the safe audit detail, and writes the outcome.
func (s *gitProxySession) runAndRespond(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
	remote resolvedRemote, branch string, args []string,
) {
	runCtx, cancel := context.WithTimeout(ctx, gitProxyNetworkTimeout)
	defer cancel()
	res, err := s.git(runCtx, args...)
	if err != nil {
		writeError(w, http.StatusBadGateway, "git_failed", err.Error())
		return
	}
	// Report the destination that was actually dialled. A remote with a
	// separate remote.<name>.pushurl reaches a different host on push than on
	// fetch, and both the audit row and the agent's own outcome must name the
	// one this command contacted.
	contacted := remote.contacted(len(args) > 0 && args[0] == "push").Key()
	// Audit detail is a short, privacy-bounded diagnostic (audit.go): the
	// remote, the ref and the outcome — never output, never a credential.
	detail := fmt.Sprintf("remote=%s ref=%s", contacted, branch)
	if branch == "" {
		detail = "remote=" + contacted
	}
	setAuditDetail(r, fmt.Sprintf("%s exit=%d", detail, res.ExitCode))
	writeJSON(w, http.StatusOK, gitProxyOutcome{
		Repo:      filepath.Base(s.repoRoot),
		Remote:    remote.Name,
		RemoteRef: contacted,
		Branch:    branch,
		ExitCode:  res.ExitCode,
		Stdout:    res.Stdout,
		Stderr:    res.Stderr,
		Truncated: res.Truncated,
		TimedOut:  res.TimedOut,
	})
}

// runIsolatedAndRespond executes a CREDENTIALED command from a daemon-owned
// transfer directory, so the agent's repository configuration is out of scope
// for the one command that carries the operator's credential. See gitProxyXfer.
func (s *gitProxySession) runIsolatedAndRespond(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
	remote resolvedRemote, branch string, args []string, setUpstream bool,
) {
	xfer, fault := newGitProxyXfer(ctx, s)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	defer xfer.cleanup()

	runCtx, cancel := context.WithTimeout(ctx, gitProxyNetworkTimeout)
	defer cancel()
	res, err := xfer.git(runCtx, s, args...)
	if err != nil {
		writeError(w, http.StatusBadGateway, "git_failed", err.Error())
		return
	}
	// --set-upstream cannot come from the pushing command any more: it writes
	// branch.<name>.* into the repository it ran in, and that is deliberately
	// not the agent's. Do it afterwards, locally, on success — it needs no
	// credential, so it is not part of what the isolation protects.
	if res.ExitCode == 0 && setUpstream {
		s.setUpstream(ctx, remote.Name, branch)
	}
	contacted := remote.contacted(true).Key()
	setAuditDetail(r, fmt.Sprintf("remote=%s ref=%s exit=%d", contacted, branch, res.ExitCode))
	writeJSON(w, http.StatusOK, gitProxyOutcome{
		Repo:      filepath.Base(s.repoRoot),
		Remote:    remote.Name,
		RemoteRef: contacted,
		Branch:    branch,
		ExitCode:  res.ExitCode,
		Stdout:    res.Stdout,
		Stderr:    res.Stderr,
		Truncated: res.Truncated,
		TimedOut:  res.TimedOut,
	})
}

// setUpstream reproduces `git push -u` in the agent's own repository. Failure
// is not fatal: the push has already landed, and an unset upstream is a
// convenience the agent can fix itself.
func (s *gitProxySession) setUpstream(ctx context.Context, remote, branch string) {
	for _, kv := range [][2]string{
		{"branch." + branch + ".remote", remote},
		{"branch." + branch + ".merge", "refs/heads/" + branch},
	} {
		_, _, _ = s.gitProbeStrict(ctx, "config", "--", kv[0], kv[1])
	}
}

// validateRefPattern bounds the optional ls-remote ref pattern. It reaches
// argv as a positional, so the leading-"-" refusal is the load-bearing rule;
// the charset keeps it a ref glob rather than anything else.
func validateRefPattern(pattern string) *proxyFault {
	if len(pattern) > maxGitProxyRefLen {
		return faultf(400, "invalid_arg", "ref pattern is longer than %d characters", maxGitProxyRefLen)
	}
	if strings.HasPrefix(pattern, "-") {
		return faultf(400, "invalid_arg", "a ref pattern may not begin with '-'")
	}
	for _, r := range pattern {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '/', r == '*':
		default:
			return faultf(400, "invalid_arg",
				"ref pattern %q contains characters outside [A-Za-z0-9._/*-]", pattern)
		}
	}
	return nil
}
