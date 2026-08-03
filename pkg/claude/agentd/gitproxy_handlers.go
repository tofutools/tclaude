package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
)

// gitproxy_handlers.go holds the HTTP surface of `tclaude agent git`. The
// hardening lives in gitproxy.go; this file is the request/response shape and
// the order of the gates.
//
// Every gate runs in the same order, and the order matters:
//
//  1. permission slug — before anything is read from the request body, so an
//     ungated caller cannot even probe for the existence of a repository;
//  2. operator policy — the proxy is off unless remotes are allow-listed;
//  3. the repo gate — daemon-recorded launch directory, no request parameter;
//  4. parameter validation — charset, length, no leading "-";
//  5. the remote gate — URL parse, allow-list, insteadOf fixed point;
//  6. only then, the subprocess.
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
	Repo           string               `json:"repo"`
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
	if err := dec.Decode(out); err != nil && err.Error() != "EOF" {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return false
	}
	return true
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
	convID, ok := requirePermission(w, r, PermGitRead)
	if !ok {
		return
	}
	ctx := r.Context()
	s, fault := newGitProxySession(ctx, convID, "")
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	resp := gitProxyRemotesResponse{
		Repo:           s.repoRoot,
		Branch:         s.currentBranch(ctx),
		Remotes:        []gitProxyRemoteView{},
		AllowedRemotes: s.policy.AllowedRemotes,
		AllowForcePush: s.policy.AllowForcePush,
		ProtectedRefs:  []string{},
	}
	if s.policy.ProtectedRefs != nil {
		resp.ProtectedRefs = *s.policy.ProtectedRefs
	}
	for i, name := range splitNonEmptyLines(s.gitProbe(ctx, "remote")) {
		if i >= maxGitProxyRemotes {
			break
		}
		view := gitProxyRemoteView{Name: name}
		if validateRemoteName(name) != nil {
			// A remote whose own name we would refuse is reported rather than
			// hidden: the agent needs to know why it cannot use it.
			view.RefusedFor = "the remote name is outside the accepted charset"
			resp.Remotes = append(resp.Remotes, view)
			continue
		}
		view.FetchURL = s.gitProbe(ctx, "remote", "get-url", "--", name)
		view.PushURL = s.gitProbe(ctx, "remote", "get-url", "--push", "--", name)
		if resolved, fault := resolveProxyRemote(ctx, s, name); fault == nil {
			view.Allowed = true
			view.RemoteRef = resolved.FetchRef.Key()
		} else {
			view.RefusedFor = fault.Msg
		}
		resp.Remotes = append(resp.Remotes, view)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGitProxyLsRemote serves POST /v1/git/ls-remote — the cheapest
// credentialed round trip, and the one an agent should use to check whether a
// branch already exists on the remote.
func handleGitProxyLsRemote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	convID, ok := requirePermission(w, r, PermGitRead)
	if !ok {
		return
	}
	var body gitProxyLsRemoteRequest
	if !decodeGitProxyBody(w, r, &body) {
		return
	}
	ctx := r.Context()
	s, resolved, fault := openProxyRemote(ctx, convID, body.Remote)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	args := []string{"ls-remote"}
	if body.Heads {
		args = append(args, "--heads")
	}
	if body.Tags {
		args = append(args, "--tags")
	}
	args = append(args, "--", resolved.Name)
	if pattern := strings.TrimSpace(body.Pattern); pattern != "" {
		if fault := validateRefPattern(pattern); fault != nil {
			writeProxyFault(w, fault)
			return
		}
		args = append(args, pattern)
	}
	s.runAndRespond(ctx, w, r, resolved, "", args)
}

// handleGitProxyFetch serves POST /v1/git/fetch. Fetch never updates the
// working tree, which is what keeps .gitattributes filter programs out of the
// daemon's blast radius — see the gitproxy.go header.
func handleGitProxyFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	convID, ok := requirePermission(w, r, PermGitRead)
	if !ok {
		return
	}
	var body gitProxyFetchRequest
	if !decodeGitProxyBody(w, r, &body) {
		return
	}
	ctx := r.Context()
	s, resolved, fault := openProxyRemote(ctx, convID, body.Remote)
	if fault != nil {
		writeProxyFault(w, fault)
		return
	}
	args := []string{"fetch"}
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
	convID, ok := requirePermission(w, r, PermGitPush)
	if !ok {
		return
	}
	var body gitProxyPushRequest
	if !decodeGitProxyBody(w, r, &body) {
		return
	}
	ctx := r.Context()
	s, resolved, fault := openProxyRemote(ctx, convID, body.Remote)
	if fault != nil {
		writeProxyFault(w, fault)
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

	args := []string{"push"}
	if body.ForceWithLease {
		if !s.policy.AllowForcePush {
			writeError(w, http.StatusForbidden, "force_push_disabled",
				"force-pushing is disabled; the operator can enable it with "+
					"agent.git_proxy.allow_force_push")
			return
		}
		// Always --force-with-lease, never --force: a lease refuses to
		// overwrite work the local repo has not seen, which is the difference
		// between "rewrite my own branch" and "destroy someone else's".
		args = append(args, "--force-with-lease")
	}
	if body.SetUpstream {
		args = append(args, "--set-upstream")
	}
	// Push by REMOTE NAME with a fully-qualified refspec. The name is safe
	// because resolveProxyRemote proved it resolves to the validated URL and is
	// a fixed point of git's rewriting; using the name (rather than the URL)
	// keeps remote-tracking refs and --set-upstream working.
	args = append(args, "--", resolved.Name,
		fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch))
	s.runAndRespond(ctx, w, r, resolved, branch, args)
}

// openProxyRemote runs gates 2, 3 and 5 for the routes that name a remote.
func openProxyRemote(ctx context.Context, convID, requestedRemote string) (
	*gitProxySession, resolvedRemote, *proxyFault,
) {
	name := remoteOrDefault(requestedRemote)
	s, fault := newGitProxySession(ctx, convID, name)
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
	// Audit detail is a short, privacy-bounded diagnostic (audit.go): the
	// remote, the ref and the outcome — never output, never a credential.
	detail := fmt.Sprintf("remote=%s ref=%s", remote.FetchRef.Key(), branch)
	if branch == "" {
		detail = "remote=" + remote.FetchRef.Key()
	}
	setAuditDetail(r, fmt.Sprintf("%s exit=%d", detail, res.ExitCode))
	writeJSON(w, http.StatusOK, gitProxyOutcome{
		Repo:      filepath.Base(s.repoRoot),
		Remote:    remote.Name,
		RemoteRef: remote.FetchRef.Key(),
		Branch:    branch,
		ExitCode:  res.ExitCode,
		Stdout:    res.Stdout,
		Stderr:    res.Stderr,
		Truncated: res.Truncated,
		TimedOut:  res.TimedOut,
	})
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
