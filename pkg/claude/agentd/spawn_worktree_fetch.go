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
	timing := worktreeStartupTiming(ctx, "worktree_fetch")
	defer timing("return")
	remote, remoteBranch, trackingRef, err := worktree.FetchTargetForBranchIn(repoRoot, base)
	if err != nil {
		return "", err
	}
	timing("fetch_target_resolved")
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
func fetchWorktreeBaseIsolated(ctx context.Context, repoRoot, remote, branch string, enforceProxyPolicy bool) (err error) {
	timing := worktreeStartupTiming(ctx, "worktree_fetch_isolated")
	defer func() { timing("return_after_cleanup", "failed", err != nil) }()
	s, fault := newGitProxySessionBase(ctx, !enforceProxyPolicy)
	if fault != nil {
		return fmt.Errorf("git proxy: %s", fault.Msg)
	}
	timing("session_prepared")
	if !enforceProxyPolicy {
		s.pins = operatorWorktreeFetchPins(s.pins)
	}
	s.repoRoot = repoRoot
	resolved, fault := resolveWorktreeFetchRemote(ctx, s, remote, enforceProxyPolicy)
	if fault != nil {
		return fmt.Errorf("git proxy: %s", fault.Msg)
	}
	timing("remote_resolved")
	xfer, fault := newGitProxyXfer(ctx, s, xferShareObjects)
	if fault != nil {
		return fmt.Errorf("git proxy: %s", fault.Msg)
	}
	timing("transfer_repo_created")
	defer xfer.cleanup()
	if fault := xfer.seedRefs(ctx, s, resolved.Name); fault != nil {
		return fmt.Errorf("git proxy: %s", fault.Msg)
	}
	timing("refs_seeded")

	args := []string{"fetch", gitProxyUploadPack, "--no-recurse-submodules", "--", resolved.FetchURL}
	args = append(args, fetchRefspecs(resolved.Name, branch, false)...)
	runCtx, cancel := context.WithTimeout(ctx, gitProxyNetworkTimeout)
	defer cancel()
	result, err := xfer.git(runCtx, s, args...)
	if err != nil {
		return fmt.Errorf("git proxy fetch %s/%s: %w", remote, branch, err)
	}
	if result.TimedOut {
		return fmt.Errorf("git proxy fetch %s/%s timed out", remote, branch)
	}
	timing("network_fetch_complete")
	imported, fault := xfer.importRefs(ctx, s, resolved.Name, false)
	if fault != nil {
		return fmt.Errorf("git proxy: %s", fault.Msg)
	}
	timing("refs_imported")
	if result.ExitCode != 0 {
		return fmt.Errorf("git proxy fetch %s/%s failed: %s", remote, branch, proxyResultMessage(result))
	}
	if imported.ExitCode != 0 {
		msg := strings.TrimSpace(imported.Stderr)
		if msg == "" {
			msg = fmt.Sprintf("git exited %d", imported.ExitCode)
		}
		return fmt.Errorf("git proxy could not import %s/%s: %s", remote, branch, msg)
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
	urls, fault := s.remoteURLs(ctx, remote, false)
	if fault != nil {
		return resolvedRemote{}, fault
	}
	if len(urls) == 0 {
		return resolvedRemote{}, faultf(404, "unknown_remote", "no remote named %q is configured", remote)
	}
	return resolvedRemote{Name: remote, FetchURL: urls[0]}, nil
}

// operatorWorktreeFetchPins keeps the execution-safety pins while allowing
// trusted global/system transport configuration in non-proxy mode. The
// credentialed command runs from a daemon-owned bare repository, so local and
// worktree config from the selected repo is already absent; clearing the
// operator's global HTTP proxy, CA, askpass, or SSH command would add no safety
// and would break common corporate/self-hosted setups.
func operatorWorktreeFetchPins(pins []string) []string {
	keep := make([]string, 0, len(pins))
	for i := 0; i < len(pins); i++ {
		if pins[i] == "-c" && i+1 < len(pins) {
			value := pins[i+1]
			if strings.HasPrefix(value, "http.proxy=") ||
				strings.HasPrefix(value, "core.sshCommand=") ||
				strings.HasPrefix(value, "core.askPass=") ||
				strings.HasPrefix(value, "core.gitProxy=") {
				i++
				continue
			}
		}
		keep = append(keep, pins[i])
	}
	return keep
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
