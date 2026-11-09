package selfupdate

import (
	"context"
	"debug/buildinfo"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A source_checkout whose ref names a branch on origin follows that branch:
// an update is any new commit on origin/<ref> compared with the commit the
// running binaries were built from (vcs.revision), not a newer release tag.
// Applying fast-forwards only, refusing a dirty tree or local commits ahead
// of origin, then builds the binaries and installs them where they live now.

func binaryRevision(path string) (string, bool, error) {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	if bi.Main.Path != module {
		return "", false, fmt.Errorf("not a tclaude binary")
	}
	rev, modified := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	return rev, modified, nil
}

func checkoutGit(ctx context.Context, path string) func(...string) (string, error) {
	sys := currentSystem()
	return func(args ...string) (string, error) {
		out, err := sys.Run(ctx, append([]string{"git", "-C", path}, args...), nil)
		return strings.TrimSpace(string(out)), err
	}
}

// branchMode fetches origin and reports whether the checkout ref is a branch
// there. A tag (or no ref) keeps the release-tag behaviour.
func (s *Service) branchMode(ctx context.Context, opts ApplyOptions) (bool, error) {
	if opts.Method != MethodSourceCheckout || opts.Checkout.Ref == "" {
		return false, nil
	}
	git := checkoutGit(ctx, opts.Checkout.Path)
	_, fetchErr := git("fetch", "--quiet", "--tags", "origin")
	_, err := git("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+opts.Checkout.Ref+"^{commit}")
	if err != nil {
		// Not a branch on origin (a tag): the release flow, which needs no
		// fetch, decides.
		return false, nil
	}
	if fetchErr != nil {
		return true, fmt.Errorf("git fetch in %s failed: %w", opts.Checkout.Path, fetchErr)
	}
	return true, nil
}

func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

func (s *Service) runBranch(ctx context.Context, req Request, job *Job, authorize func() bool, opts ApplyOptions) error {
	ref, path := opts.Checkout.Ref, opts.Checkout.Path
	git := checkoutGit(ctx, path)
	if err := s.phase(job, "checking_branch"); err != nil {
		return err
	}
	remote := "refs/remotes/origin/" + ref
	target, err := git("rev-parse", "--verify", remote+"^{commit}")
	if err != nil {
		return fmt.Errorf("cannot resolve origin/%s: %s", ref, tail([]byte(target), 256))
	}
	running := primary(s.binaries).Revision
	// A build is behind when origin/<ref> contains its commit. A build that
	// is ahead of or diverged from origin (an unpushed fix installed by
	// hand) is never replaced with an older commit.
	behind := func(rev string) (bool, bool) {
		switch rev {
		case "":
			return true, false
		case target:
			return false, false
		}
		_, err := git("merge-base", "--is-ancestor", rev, target)
		return err == nil, err != nil
	}
	stale := false
	for _, b := range s.binaries {
		isBehind, diverged := behind(b.Revision)
		if diverged {
			job.Warnings = append(job.Warnings, fmt.Sprintf("%s is built from %s, which is not on origin/%s (ahead or diverged); it is left as is", b.Name, shortRev(b.Revision), ref))
			continue
		}
		stale = stale || isBehind || b.Revision == target && b.Modified
	}
	var commits []string
	if running != "" && running != target {
		if out, err := git("log", "--oneline", "--no-decorate", "--max-count=50", running+".."+target); err == nil && out != "" {
			commits = strings.Split(out, "\n")
		} else if err != nil {
			job.Warnings = append(job.Warnings, "the running build's commit "+shortRev(running)+" is not in the checkout; listing no commits")
		}
	}
	if running == "" {
		job.Warnings = append(job.Warnings, "the running build has no VCS revision; it is treated as out of date")
	}
	s.mu.Lock()
	now := time.Now().UTC()
	job.Version = ref + "@" + shortRev(target)
	job.Revision, job.Commits, job.UpdateAvailable = target, commits, &stale
	if !stale {
		commits = nil
		job.Commits = nil
	}
	s.status.Branch, s.status.LatestRevision, s.status.PendingCommits, s.status.CheckedAt = ref, target, commits, &now
	s.status.UpdateAvailable = &stale
	err = s.saveJob(*job)
	s.mu.Unlock()
	if err != nil || req.Action != "apply" || !stale {
		return err
	}

	if err := s.phase(job, "fast_forwarding"); err != nil {
		return err
	}
	status, err := git("status", "--porcelain")
	if err != nil {
		return fmt.Errorf("%s is not a usable git checkout: %s", path, tail([]byte(status), 512))
	}
	if status != "" {
		return fmt.Errorf("the checkout %s has uncommitted changes; commit or stash them first (tclaude never touches them)", path)
	}
	if head, _ := git("symbolic-ref", "--short", "-q", "HEAD"); head != ref {
		if head == "" {
			head = "a detached HEAD"
		}
		return fmt.Errorf("the checkout %s is on %s, not %s; tclaude does not switch branches for you", path, head, ref)
	}
	ahead, err := git("rev-list", "--count", remote+"..refs/heads/"+ref)
	if err != nil || ahead != "0" {
		return fmt.Errorf("the local %s in %s has commits that are not on origin/%s; tclaude only fast-forwards", ref, path, ref)
	}
	if out, err := git("merge", "--ff-only", "--quiet", remote); err != nil {
		return fmt.Errorf("could not fast-forward %s: %s", ref, tail([]byte(out), 512))
	}
	if head, err := git("rev-parse", "HEAD"); err != nil || head != target {
		return fmt.Errorf("the checkout is at %s after fast-forwarding, not origin/%s %s", shortRev(head), ref, shortRev(target))
	}

	if err := s.phase(job, "staging"); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(s.dir, "stage-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	built, err := s.buildCheckout(ctx, path, dir)
	if err != nil {
		return err
	}
	targets := []applyTarget{}
	seen := map[string]bool{}
	for _, b := range s.binaries {
		if seen[b.Name] {
			continue
		}
		seen[b.Name] = true
		t := applyTarget{binary: b, dest: b.Path, staged: built[b.Name]}
		if t.staged == "" {
			return fmt.Errorf("the checkout build did not produce %s", b.Name)
		}
		if b.ManagedBy != "" {
			t.dest = filepath.Join(opts.InstallDir, b.Name)
		}
		// A branch build has a pseudo-version; it is verified by commit.
		rev, modified, err := s.revisionOf(t.staged)
		if err != nil {
			return fmt.Errorf("cannot read the build of %s: %w", b.Name, err)
		}
		if rev != target || modified {
			return fmt.Errorf("the build of %s is from %s (modified: %t), not origin/%s %s", b.Name, shortRev(rev), modified, ref, shortRev(target))
		}
		info, err := s.inspect(ctx, t.staged)
		if err != nil {
			return err
		}
		if info.ProtocolVersion != s.status.ProtocolVersion {
			return fmt.Errorf("target federation protocol %d differs from current %d; linked peers may be incompatible", info.ProtocolVersion, s.status.ProtocolVersion)
		}
		if err := s.preflight(ctx, job, b.Name, t.staged, opts); err != nil {
			return err
		}
		targets = append(targets, t)
	}
	if err := s.install(ctx, Release{Tag: job.Version}, job, authorize, targets, nil, opts); err != nil {
		return err
	}
	// The binaries on disk may already be at the target (updated by hand)
	// while this daemon still runs the older build: restart into them.
	job.RestartRequired = true
	return nil
}
