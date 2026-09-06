//go:build linux || darwin

package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrCheckoutUnavailable      = errors.New("checkout is unavailable")
	ErrCheckoutIdentityMismatch = errors.New("checkout identity does not match ownership evidence")
	ErrCheckoutNotOwned         = errors.New("checkout is not owned by the platform")
	ErrCheckoutDirty            = errors.New("checkout has uncommitted changes")
	ErrCheckoutInUse            = errors.New("checkout is in use")
	ErrCheckoutShared           = errors.New("checkout is shared")
	ErrCheckoutMain             = errors.New("main checkout cannot be removed")
	ErrCheckoutBaseConflict     = errors.New("existing checkout branch does not match selected base")
)

type CheckoutOwnership string

const (
	CheckoutRegistered CheckoutOwnership = "registered"
	CheckoutCreated    CheckoutOwnership = "created"
	CheckoutAdopted    CheckoutOwnership = "adopted"
)

type CheckoutEffectState string

const (
	CheckoutEffectReady     CheckoutEffectState = "ready"
	CheckoutEffectUncertain CheckoutEffectState = "uncertain"
	CheckoutEffectAbsent    CheckoutEffectState = "absent"
)

// CheckoutIntent is an application-resolved request for an isolated Git
// checkout. Repository and Path are explicit: the host never derives
// ownership or cleanup authority from a path naming convention.
type CheckoutIntent struct {
	Repository string
	Path       string
	Branch     string
	Base       string
}

// CheckoutEvidence is opaque recovery material to application callers. The
// host revalidates Git's worktree identity and its private ownership marker
// before restoring or removing anything.
type CheckoutEvidence struct {
	Ownership     CheckoutOwnership `json:"ownership"`
	Repository    string            `json:"repository"`
	Path          string            `json:"path"`
	GitCommonDir  string            `json:"git_common_dir"`
	GitDir        string            `json:"git_dir"`
	Branch        string            `json:"branch"`
	Base          string            `json:"base,omitempty"`
	InitialCommit string            `json:"initial_commit,omitempty"`
	OwnerToken    string            `json:"owner_token,omitempty"`
	Linked        bool              `json:"linked"`
	CreatedAt     time.Time         `json:"created_at,omitempty"`
}

type CheckoutObservation struct {
	State    CheckoutEffectState
	Evidence CheckoutEvidence
	Commit   string
	Dirty    bool
}

type CheckoutCreateResult struct {
	State    CheckoutEffectState
	Evidence CheckoutEvidence
}

type CheckoutRemovalRequest struct {
	Evidence    CheckoutEvidence
	UseCount    int
	Shared      bool
	Destructive bool
}

type CheckoutRemovalResult struct {
	State    CheckoutEffectState
	Evidence CheckoutEvidence
}

type CheckoutHost struct {
	git string
}

func NewCheckoutHost(gitExecutable string) (CheckoutHost, error) {
	if gitExecutable == "" {
		gitExecutable = "git"
	}
	resolved, err := exec.LookPath(gitExecutable)
	if err != nil {
		return CheckoutHost{}, fmt.Errorf("resolve Git executable: %w", err)
	}
	return CheckoutHost{git: resolved}, nil
}

func (h CheckoutHost) Register(ctx context.Context, path string) (CheckoutEvidence, error) {
	return h.record(ctx, path, CheckoutRegistered, "", "", "")
}

func (h CheckoutHost) Adopt(ctx context.Context, path string) (CheckoutEvidence, error) {
	evidence, err := h.record(ctx, path, CheckoutAdopted, "", "", "")
	if err != nil {
		return CheckoutEvidence{}, err
	}
	if err := writeCheckoutOwner(evidence.GitDir, evidence.OwnerToken); err != nil {
		return CheckoutEvidence{}, err
	}
	return evidence, nil
}

func (h CheckoutHost) Create(ctx context.Context, intent CheckoutIntent) (CheckoutCreateResult, error) {
	repository, err := h.repositoryRoot(ctx, intent.Repository)
	if err != nil {
		return CheckoutCreateResult{}, err
	}
	target, err := cleanAbsolute(intent.Path)
	if err != nil {
		return CheckoutCreateResult{}, fmt.Errorf("checkout path: %w", err)
	}
	branch := strings.TrimSpace(intent.Branch)
	if branch == "" {
		return CheckoutCreateResult{}, fmt.Errorf("checkout branch is required")
	}
	if _, err := os.Lstat(target); err == nil {
		return CheckoutCreateResult{}, fmt.Errorf("checkout path already exists: %s", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return CheckoutCreateResult{}, fmt.Errorf("inspect checkout path: %w", err)
	}
	base := strings.TrimSpace(intent.Base)
	if base == "" {
		base = "HEAD"
	}
	baseCommit, err := h.gitOutput(ctx, repository, "rev-parse", "--verify", base+"^{commit}")
	if err != nil {
		return CheckoutCreateResult{}, fmt.Errorf("resolve checkout base %q: %w", base, err)
	}
	common, err := h.gitOutput(ctx, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return CheckoutCreateResult{}, fmt.Errorf("resolve Git common directory: %w", err)
	}
	provisional := CheckoutEvidence{
		Ownership: CheckoutCreated, Repository: repository, Path: target,
		GitCommonDir: filepath.Clean(common), Branch: branch, Base: base,
		InitialCommit: strings.TrimSpace(baseCommit), CreatedAt: time.Now().UTC(),
	}
	branchExists := h.refExists(ctx, repository, "refs/heads/"+branch)
	args := []string{"-c", "core.hooksPath=/dev/null", "worktree", "add"}
	if branchExists {
		branchCommit, resolveErr := h.gitOutput(ctx, repository, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
		if resolveErr != nil {
			return CheckoutCreateResult{}, resolveErr
		}
		if strings.TrimSpace(branchCommit) != provisional.InitialCommit {
			return CheckoutCreateResult{}, fmt.Errorf("%w: branch %q is at %s, selected base is %s",
				ErrCheckoutBaseConflict, branch, strings.TrimSpace(branchCommit), provisional.InitialCommit)
		}
		args = append(args, target, branch)
	} else {
		// Pin the resolved commit rather than passing mutable base spelling
		// across the effect boundary.
		args = append(args, "-b", branch, target, provisional.InitialCommit)
	}
	if _, err := h.gitOutput(ctx, repository, args...); err != nil {
		// Git may have created the branch or checkout before returning an
		// error. Return the intended resource evidence so application can
		// persist and reconcile the partial effect instead of replaying it.
		if observed, inspectErr := h.record(ctx, target, CheckoutCreated, base, provisional.InitialCommit, ""); inspectErr == nil {
			provisional = observed
		}
		return CheckoutCreateResult{State: CheckoutEffectUncertain, Evidence: provisional}, fmt.Errorf("create Git checkout: %w", err)
	}
	evidence, err := h.record(ctx, target, CheckoutCreated, base, provisional.InitialCommit, "")
	if err != nil {
		return CheckoutCreateResult{State: CheckoutEffectUncertain, Evidence: provisional}, fmt.Errorf("inspect created Git checkout: %w", err)
	}
	if err := writeCheckoutOwner(evidence.GitDir, evidence.OwnerToken); err != nil {
		return CheckoutCreateResult{State: CheckoutEffectUncertain, Evidence: evidence}, err
	}
	return CheckoutCreateResult{State: CheckoutEffectReady, Evidence: evidence}, nil
}

func (h CheckoutHost) Inspect(ctx context.Context, evidence CheckoutEvidence) (CheckoutObservation, error) {
	if _, err := os.Stat(evidence.Path); errors.Is(err, os.ErrNotExist) {
		return CheckoutObservation{State: CheckoutEffectAbsent, Evidence: evidence}, nil
	} else if err != nil {
		return CheckoutObservation{State: CheckoutEffectUncertain, Evidence: evidence}, err
	}
	current, err := h.record(ctx, evidence.Path, evidence.Ownership, evidence.Base, evidence.InitialCommit, evidence.OwnerToken)
	if err != nil {
		return CheckoutObservation{State: CheckoutEffectUncertain, Evidence: evidence}, err
	}
	if !sameCheckoutIdentity(evidence, current) {
		return CheckoutObservation{State: CheckoutEffectUncertain, Evidence: current}, ErrCheckoutIdentityMismatch
	}
	if evidence.Ownership != CheckoutRegistered {
		if err := verifyCheckoutOwner(current.GitDir, evidence.OwnerToken); err != nil {
			return CheckoutObservation{State: CheckoutEffectUncertain, Evidence: current}, err
		}
	}
	commit, err := h.gitOutput(ctx, current.Path, "rev-parse", "HEAD")
	if err != nil {
		return CheckoutObservation{State: CheckoutEffectUncertain, Evidence: current}, err
	}
	status, err := h.gitOutput(ctx, current.Path, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return CheckoutObservation{State: CheckoutEffectUncertain, Evidence: current}, err
	}
	return CheckoutObservation{State: CheckoutEffectReady, Evidence: current, Commit: strings.TrimSpace(commit), Dirty: status != ""}, nil
}

func (h CheckoutHost) Restore(ctx context.Context, evidence CheckoutEvidence) (CheckoutCreateResult, error) {
	if evidence.Ownership == CheckoutRegistered || evidence.OwnerToken == "" {
		return CheckoutCreateResult{}, ErrCheckoutNotOwned
	}
	observed, err := h.Inspect(ctx, evidence)
	if err == nil && observed.State == CheckoutEffectReady {
		return CheckoutCreateResult{State: CheckoutEffectReady, Evidence: observed.Evidence}, nil
	}
	if err != nil && !errors.Is(err, ErrCheckoutUnavailable) {
		return CheckoutCreateResult{State: CheckoutEffectUncertain, Evidence: evidence}, err
	}
	if observed.State != CheckoutEffectAbsent {
		return CheckoutCreateResult{State: CheckoutEffectUncertain, Evidence: evidence}, ErrCheckoutIdentityMismatch
	}
	if verifyErr := verifyCheckoutOwner(evidence.GitDir, evidence.OwnerToken); verifyErr != nil {
		return CheckoutCreateResult{State: CheckoutEffectUncertain, Evidence: evidence}, verifyErr
	}
	if evidence.Branch == "" || evidence.Branch == "HEAD" || !h.refExists(ctx, evidence.Repository, "refs/heads/"+evidence.Branch) {
		return CheckoutCreateResult{State: CheckoutEffectUncertain, Evidence: evidence}, ErrCheckoutIdentityMismatch
	}
	if evidence.InitialCommit != "" {
		if _, err := h.gitOutput(ctx, evidence.Repository, "merge-base", "--is-ancestor", evidence.InitialCommit, "refs/heads/"+evidence.Branch); err != nil {
			return CheckoutCreateResult{State: CheckoutEffectUncertain, Evidence: evidence}, fmt.Errorf("%w: retained branch no longer descends from its initial commit", ErrCheckoutIdentityMismatch)
		}
	}
	// Git --force repairs the exact missing registration while checking out the
	// retained branch at its current committed head. Unlike remove+Create this
	// does not compare an advanced worker branch to its creation commit.
	if _, err := h.gitOutput(ctx, evidence.Repository, "worktree", "add", "--force", evidence.Path, evidence.Branch); err != nil {
		// Some Git failures replace the admin directory before returning. Keep
		// the previously validated receipt recoverable whenever its exact admin
		// identity still exists.
		_ = writeCheckoutOwner(evidence.GitDir, evidence.OwnerToken)
		return CheckoutCreateResult{State: CheckoutEffectUncertain, Evidence: evidence}, fmt.Errorf("restore Git checkout: %w", err)
	}
	restored, err := h.record(ctx, evidence.Path, evidence.Ownership, evidence.Base, evidence.InitialCommit, evidence.OwnerToken)
	if err != nil || !sameCheckoutIdentity(evidence, restored) {
		_ = writeCheckoutOwner(evidence.GitDir, evidence.OwnerToken)
		if err == nil {
			err = ErrCheckoutIdentityMismatch
		}
		return CheckoutCreateResult{State: CheckoutEffectUncertain, Evidence: evidence}, err
	}
	if err := writeCheckoutOwner(restored.GitDir, evidence.OwnerToken); err != nil {
		return CheckoutCreateResult{State: CheckoutEffectUncertain, Evidence: restored}, err
	}
	return CheckoutCreateResult{State: CheckoutEffectReady, Evidence: restored}, nil
}

func (h CheckoutHost) Remove(ctx context.Context, request CheckoutRemovalRequest) (CheckoutRemovalResult, error) {
	evidence := request.Evidence
	if evidence.Ownership == CheckoutRegistered || evidence.OwnerToken == "" {
		return CheckoutRemovalResult{State: CheckoutEffectUncertain, Evidence: evidence}, ErrCheckoutNotOwned
	}
	if !evidence.Linked {
		return CheckoutRemovalResult{State: CheckoutEffectReady, Evidence: evidence}, ErrCheckoutMain
	}
	observation, err := h.Inspect(ctx, evidence)
	if err != nil {
		return CheckoutRemovalResult{State: CheckoutEffectUncertain, Evidence: evidence}, err
	}
	if observation.State == CheckoutEffectAbsent {
		return CheckoutRemovalResult{State: CheckoutEffectAbsent, Evidence: evidence}, nil
	}
	if !request.Destructive {
		switch {
		case request.UseCount > 0:
			return CheckoutRemovalResult{State: CheckoutEffectReady, Evidence: evidence}, ErrCheckoutInUse
		case request.Shared:
			return CheckoutRemovalResult{State: CheckoutEffectReady, Evidence: evidence}, ErrCheckoutShared
		case observation.Dirty:
			return CheckoutRemovalResult{State: CheckoutEffectReady, Evidence: evidence}, ErrCheckoutDirty
		}
	}
	args := []string{"worktree", "remove"}
	if request.Destructive {
		args = append(args, "--force")
	}
	args = append(args, evidence.Path)
	if _, err := h.gitOutput(ctx, evidence.Repository, args...); err != nil {
		return CheckoutRemovalResult{State: CheckoutEffectUncertain, Evidence: evidence}, fmt.Errorf("remove Git checkout: %w", err)
	}
	return CheckoutRemovalResult{State: CheckoutEffectAbsent, Evidence: evidence}, nil
}

func (h CheckoutHost) record(ctx context.Context, path string, ownership CheckoutOwnership, base, initialCommit, ownerToken string) (CheckoutEvidence, error) {
	root, err := h.repositoryRoot(ctx, path)
	if err != nil {
		return CheckoutEvidence{}, ErrCheckoutUnavailable
	}
	common, err := h.gitOutput(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return CheckoutEvidence{}, err
	}
	gitDir, err := h.gitOutput(ctx, root, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return CheckoutEvidence{}, err
	}
	branch, err := h.gitOutput(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		branch = "HEAD"
	}
	repository := filepath.Dir(filepath.Clean(common))
	if ownerToken == "" && ownership != CheckoutRegistered {
		ownerToken, err = checkoutOwnerToken()
		if err != nil {
			return CheckoutEvidence{}, err
		}
	}
	return CheckoutEvidence{
		Ownership: ownership, Repository: repository, Path: root,
		GitCommonDir: filepath.Clean(common), GitDir: filepath.Clean(gitDir),
		Branch: strings.TrimSpace(branch), Base: base, InitialCommit: initialCommit,
		OwnerToken: ownerToken, Linked: filepath.Clean(common) != filepath.Clean(gitDir), CreatedAt: time.Now().UTC(),
	}, nil
}

func (h CheckoutHost) repositoryRoot(ctx context.Context, path string) (string, error) {
	path, err := cleanAbsolute(path)
	if err != nil {
		return "", err
	}
	root, err := h.gitOutput(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("resolve Git repository at %s: %w", path, err)
	}
	return cleanAbsolute(root)
}

func (h CheckoutHost) refExists(ctx context.Context, repository, ref string) bool {
	cmd := exec.CommandContext(ctx, h.git, "-C", repository, "show-ref", "--verify", "--quiet", ref)
	return cmd.Run() == nil
}

func (h CheckoutHost) gitOutput(ctx context.Context, directory string, args ...string) (string, error) {
	argv := append([]string{"-C", directory}, args...)
	cmd := exec.CommandContext(ctx, h.git, argv...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func cleanAbsolute(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if abs == string(filepath.Separator) {
		return "", fmt.Errorf("root path is not a valid resource")
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs, nil
}

func sameCheckoutIdentity(want, got CheckoutEvidence) bool {
	return want.Path == got.Path && want.GitCommonDir == got.GitCommonDir && want.GitDir == got.GitDir
}

const checkoutOwnerFile = "tclaude-checkout-owner"

func checkoutOwnerToken() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func writeCheckoutOwner(gitDir, token string) error {
	if token == "" {
		return fmt.Errorf("checkout owner token is required")
	}
	path := filepath.Join(gitDir, checkoutOwnerFile)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("record checkout ownership: %w", err)
	}
	if _, err := file.WriteString(token); err != nil {
		_ = file.Close()
		return fmt.Errorf("record checkout ownership: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("record checkout ownership: %w", err)
	}
	return nil
}

func verifyCheckoutOwner(gitDir, token string) error {
	value, err := os.ReadFile(filepath.Join(gitDir, checkoutOwnerFile))
	if err != nil || string(value) != token {
		return ErrCheckoutIdentityMismatch
	}
	return nil
}
