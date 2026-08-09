package agentd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const (
	codexStateRootSourceCodexHome = "CODEX_HOME"
	codexStateRootSourceHome      = "HOME"
)

// codexStateRootForLaunch resolves the store the launched Codex process will
// use, then translates a sandbox guest path back to the host path the wrapper
// must use to resolve the conversation before the sandbox exists.
func codexStateRootForLaunch(harnessName string, snapshot *sandboxpolicy.Snapshot) (string, string, error) {
	if harnessOrDefault(harnessName) != harness.CodexName {
		return "", "", nil
	}
	environment := map[string]string{
		"CODEX_HOME": strings.TrimSpace(os.Getenv("CODEX_HOME")),
		"HOME":       strings.TrimSpace(os.Getenv("HOME")),
	}
	if snapshot != nil {
		for _, entry := range snapshot.Effective.Environment {
			if entry.Name == "CODEX_HOME" || entry.Name == "HOME" {
				environment[entry.Name] = strings.TrimSpace(entry.Value)
			}
		}
	}
	root, source := environment["CODEX_HOME"], codexStateRootSourceCodexHome
	if root == "" {
		home := environment["HOME"]
		if home == "" {
			var err error
			home, err = os.UserHomeDir()
			if err != nil {
				return "", "", fmt.Errorf("resolve Codex HOME: %w", err)
			}
		}
		root, source = filepath.Join(home, ".codex"), codexStateRootSourceHome
	}
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return "", "", fmt.Errorf("effective %s path %q is not absolute", source, root)
	}
	if snapshot != nil {
		root = hostPathForSandboxGuest(root, snapshot.Effective.Filesystem)
	}
	return root, source, nil
}

func hostPathForSandboxGuest(path string, grants []sandboxpolicy.FilesystemGrant) string {
	bestGuest := ""
	bestHost := ""
	for _, grant := range grants {
		if grant.Access == sandboxpolicy.AccessDeny || !grant.IsRemapped() {
			continue
		}
		guest := filepath.Clean(grant.GuestPath())
		if !pathWithin(path, guest) || len(guest) <= len(bestGuest) {
			continue
		}
		bestGuest, bestHost = guest, filepath.Clean(grant.Path)
	}
	if bestGuest == "" {
		return path
	}
	rel, err := filepath.Rel(bestGuest, path)
	if err != nil || rel == "." {
		return bestHost
	}
	return filepath.Join(bestHost, rel)
}

func pathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func environmentWithCodexStateRoot(environment []string, root string) []string {
	root = strings.TrimSpace(root)
	if root == "" {
		return environment
	}
	out := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if strings.SplitN(entry, "=", 2)[0] != "CODEX_HOME" {
			out = append(out, entry)
		}
	}
	return append(out, "CODEX_HOME="+root)
}
