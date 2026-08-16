package agentd

import (
	"context"
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

// fetchLatestWorktreeBase refreshes the selected base branch and returns the
// remote-tracking ref that now names the fetched commit. Returning that ref is
// essential: fetch updates refs/remotes/<remote>/..., not the checked-out local
// main, so cutting from the local name would still produce a stale worktree.
func fetchLatestWorktreeBase(ctx context.Context, repoRoot, base string) (string, error) {
	remote, remoteBranch, trackingRef, err := worktree.FetchTargetForBranchIn(repoRoot, base)
	if err != nil {
		return "", err
	}
	cfg, err := config.Load()
	if err != nil {
		return "", fmt.Errorf("read Git proxy configuration: %w", err)
	}
	if err := fetchWorktreeBaseIsolated(ctx, repoRoot, remote, remoteBranch, cfg.GitProxyEnabled()); err != nil {
		return "", err
	}
	return trackingRef, nil
}

// fetchWorktreeBaseIsolated reuses the proxy's isolated transfer, credential
// pins, hostile-config gates, object sharing, timeout, and compare-and-swap ref
// import for every dashboard fetch. When the Git proxy is enabled its remote
// allow-list is enforced too. When disabled the operator-authenticated picker
// does not add an allow-list, but the transfer still permits only the proxy's
// hardened HTTPS and SSH transports and never runs a credentialed command in
// the agent-writable repository.
func fetchWorktreeBaseIsolated(ctx context.Context, repoRoot, remote, branch string, enforceProxyPolicy bool) error {
	s, fault := newGitProxySessionBase(ctx, !enforceProxyPolicy)
	if fault != nil {
		return fmt.Errorf("Git proxy: %s", fault.Msg)
	}
	s.repoRoot = repoRoot
	resolved, fault := resolveWorktreeFetchRemote(ctx, s, remote, enforceProxyPolicy)
	if fault != nil {
		return fmt.Errorf("Git proxy: %s", fault.Msg)
	}
	xfer, fault := newGitProxyXfer(ctx, s, xferShareObjects)
	if fault != nil {
		return fmt.Errorf("Git proxy: %s", fault.Msg)
	}
	defer xfer.cleanup()
	if fault := xfer.seedRefs(ctx, s, resolved.Name); fault != nil {
		return fmt.Errorf("Git proxy: %s", fault.Msg)
	}

	args := []string{"fetch", gitProxyUploadPack, "--no-recurse-submodules", "--", resolved.FetchURL}
	args = append(args, fetchRefspecs(resolved.Name, branch, false)...)
	runCtx, cancel := context.WithTimeout(ctx, gitProxyNetworkTimeout)
	defer cancel()
	result, err := xfer.git(runCtx, s, args...)
	if err != nil {
		return fmt.Errorf("Git proxy fetch %s/%s: %w", remote, branch, err)
	}
	if result.TimedOut {
		return fmt.Errorf("Git proxy fetch %s/%s timed out", remote, branch)
	}
	imported, fault := xfer.importRefs(ctx, s, resolved.Name, false)
	if fault != nil {
		return fmt.Errorf("Git proxy: %s", fault.Msg)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("Git proxy fetch %s/%s failed: %s", remote, branch, proxyResultMessage(result))
	}
	if imported.ExitCode != 0 {
		msg := strings.TrimSpace(imported.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("git exited %d", imported.ExitCode)
		}
		return fmt.Errorf("Git proxy could not import %s/%s: %s", remote, branch, msg)
	}
	return nil
}

func resolveWorktreeFetchRemote(ctx context.Context, s *gitProxySession, remote string, enforceProxyPolicy bool) (resolvedRemote, *proxyFault) {
	if enforceProxyPolicy {
		return resolveProxyRemote(ctx, s, remote)
	}
	if fault := validateRemoteName(remote); fault != nil {
		return resolvedRemote{}, fault
	}
	if fault := refuseHostileRepoConfig(ctx, s, remote); fault != nil {
		return resolvedRemote{}, fault
	}
	urls, fault := s.remoteURLs(ctx, remote, false)
	if fault != nil {
		return resolvedRemote{}, fault
	}
	if len(urls) == 0 {
		return resolvedRemote{}, faultf(404, "unknown_remote", "no remote named %q is configured", remote)
	}
	return resolvedRemote{Name: remote, FetchURL: urls[0]}, nil
}

func proxyResultMessage(result ProxyResult) string {
	if msg := strings.TrimSpace(result.Stderr); msg != "" {
		return msg
	}
	if msg := strings.TrimSpace(result.Stdout); msg != "" {
		return msg
	}
	return fmt.Sprintf("git exited %d", result.ExitCode)
}
