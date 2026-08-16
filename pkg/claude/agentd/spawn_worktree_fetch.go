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
	if cfg.GitProxyEnabled() {
		if err := fetchWorktreeBaseThroughProxy(ctx, repoRoot, remote, remoteBranch); err != nil {
			return "", err
		}
	} else if err := worktree.FetchBranchIn(repoRoot, remote, remoteBranch); err != nil {
		return "", fmt.Errorf("fetch %s/%s: %w", remote, remoteBranch, err)
	}
	return trackingRef, nil
}

// fetchWorktreeBaseThroughProxy reuses the proxy's isolated transfer, URL
// allow-list, credential pins, hostile-config gates, object sharing, and
// compare-and-swap ref import. The only different seam is repository
// selection: this endpoint is operator-authenticated, so its already-resolved
// dashboard repo replaces an agent's daemon-recorded launch repo.
func fetchWorktreeBaseThroughProxy(ctx context.Context, repoRoot, remote, branch string) error {
	s, fault := newGitProxySessionBase(ctx, false)
	if fault != nil {
		return fmt.Errorf("Git proxy: %s", fault.Msg)
	}
	s.repoRoot = repoRoot
	resolved, fault := resolveProxyRemote(ctx, s, remote)
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

func proxyResultMessage(result ProxyResult) string {
	if msg := strings.TrimSpace(result.Stderr); msg != "" {
		return msg
	}
	if msg := strings.TrimSpace(result.Stdout); msg != "" {
		return msg
	}
	return fmt.Sprintf("git exited %d", result.ExitCode)
}
