package agentd

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// livePresentedPRAccessValidator proves that the PR belongs to a repository
// inside the caller's daemon-recorded launch tree, and that the operator has
// allow-listed that repository for credentialed GitHub access.
func livePresentedPRAccessValidator(ctx context.Context, caller, requestedDir, rawURL string) (string, error) {
	prRemote, prRepo, err := presentedPRRemoteOf(rawURL)
	if err != nil {
		return "", err
	}
	if err := presentedPRRemotePolicyCheck(rawURL); err != nil {
		return "", err
	}

	dir := strings.TrimSpace(requestedDir)
	if dir == "" {
		roots := callerOwnedTrustRoots(caller)
		if len(roots) == 0 {
			return "", fmt.Errorf("the calling agent has no recorded launch directory")
		}
		dir = roots[0]
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || !filepath.IsAbs(resolved) || !callerOwnedDirTrust(caller, resolved) {
		return "", fmt.Errorf("repository directory must be the calling agent's launch directory or a subdirectory of it")
	}

	gitPath, err := proxyBinary("git")
	if err != nil {
		return "", err
	}
	repoRoot, fault := resolveProxyRepoAt(ctx, gitPath, caller, resolved)
	if fault != nil {
		return "", fmt.Errorf("could not validate the calling agent's repository: %s", fault.Msg)
	}
	remote := strings.TrimSpace(runInDir(repoRoot, "git", "remote", "get-url", "origin"))
	remoteParsed, err := parseRemoteURL(remote)
	if err != nil {
		return "", fmt.Errorf("could not validate repository origin: %w", err)
	}
	if !strings.EqualFold(remoteParsed.Key(), prRemote.Key()) {
		return "", fmt.Errorf("pull request repository %s does not match repository origin %s", prRepo, remoteParsed.Key())
	}
	return repoRoot, nil
}

// presentedPRRemoteOf renders the repository a pull or merge request URL
// names as a remote reference, for comparison against the caller's origin.
func presentedPRRemoteOf(rawURL string) (remoteRef, string, error) {
	if ref, ok := githubPRRefFromURL(rawURL); ok {
		return remoteRef{Scheme: "https", Host: "github.com", Path: strings.Split(ref.repo, "/")}, "github.com/" + ref.repo, nil
	}
	if ref, ok := gitlabMRRefFromURL(rawURL); ok {
		return ref.remote(), ref.host + "/" + ref.project, nil
	}
	return remoteRef{}, "", fmt.Errorf("the URL does not identify a GitHub pull request or a GitLab merge request")
}

func presentedPRViewArgs(rawURL, fields string, strict bool) ([]string, bool) {
	if !strict {
		u, err := url.Parse(strings.TrimSpace(rawURL))
		if err != nil || (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) || u.Host == "" {
			return nil, false
		}
		return []string{"pr", "view", strings.TrimSpace(rawURL), "--json", fields}, true
	}
	ref, ok := githubPRRefFromURL(rawURL)
	if !ok {
		return nil, false
	}
	return []string{"pr", "view", strconv.Itoa(ref.number), "--repo", ref.repo, "--json", fields}, true
}

func validatePresentedPRRemotePolicy(rawURL string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("could not load git proxy policy: %w", err)
	}
	if !cfg.GitProxyEnabled() {
		return nil
	}
	policy := cfg.ResolvedGitProxy()
	if glRef, ok := gitlabMRRefFromURL(rawURL); ok {
		// The instance must be configured and the project allow-listed; both
		// are the same gate the GitLab proxy applies before spending the token.
		_, err := gitlabMRPolicyBase(glRef, policy)
		return err
	}
	ref, ok := githubPRRefFromURL(rawURL)
	if !ok {
		return fmt.Errorf("the URL does not identify a GitHub pull request or a GitLab merge request")
	}
	if !presentedPRRemoteAllowed(ref, policy.AllowedRemotes) {
		return fmt.Errorf("repository github.com/%s is not on the operator's agent.git_proxy.allowed_remotes list", ref.repo)
	}
	return nil
}

func presentedPRRemoteAllowed(ref githubPRRef, allowed []string) bool {
	remote := remoteRef{Scheme: "https", Host: "github.com", Path: strings.Split(ref.repo, "/")}
	return len(allowed) > 0 && remoteAllowed(remote, allowed)
}
