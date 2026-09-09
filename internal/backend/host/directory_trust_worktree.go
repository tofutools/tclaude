//go:build linux || darwin

package host

import (
	"context"
	"path/filepath"
	"strings"
)

func (p DirectoryProof) DefaultSiblingTrustDirectories(ctx context.Context, cwd string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	git, err := NewCheckoutHost("")
	if err != nil {
		// Git is optional for ordinary native launches; without it there is no
		// automatic sibling-worktree exception to apply.
		return nil, nil
	}
	common, err := git.gitOutput(ctx, cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, nil
	}
	if !filepath.IsAbs(common) || filepath.Base(common) != ".git" {
		return nil, nil
	}
	main := filepath.Dir(common)
	if filepath.Dir(cwd) != filepath.Dir(main) || cwd == main || !strings.HasPrefix(filepath.Base(cwd), filepath.Base(main)+"-") {
		return nil, nil
	}
	admin, err := git.gitOutput(ctx, cwd, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return nil, err
	}
	return p.ResolveProofDirectories(ctx, []string{cwd, admin})
}
