package agentd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const groupRepositoryCloneTimeout = 10 * time.Minute

var githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// groupRepositoryClone is the optional dashboard-only workspace provisioning
// request shared by blank-group creation and template instantiation. The
// daemon deliberately derives the actual remote URL from the canonical
// owner/repo identity plus the selected transport; it never executes a
// caller-supplied command or forwards arbitrary git options.
type groupRepositoryClone struct {
	Repository  string `json:"repository"`
	Transport   string `json:"transport"`
	Destination string `json:"destination"`
	Attach      bool   `json:"attach,omitempty"`
}

type preparedGroupRepositoryClone struct {
	CloneURL    string
	WebURL      string
	Label       string
	Destination string
}

// prepareGroupRepositoryClone validates and normalises without touching the
// filesystem. Only GitHub is supported by the first UI iteration; accepting
// its common copy/paste URL shapes keeps the input friendly while the strict
// owner/repo result keeps clone destinations and attachment URLs predictable.
func prepareGroupRepositoryClone(raw *groupRepositoryClone) (*preparedGroupRepositoryClone, error) {
	if raw == nil {
		return nil, nil
	}
	repository := strings.TrimSpace(raw.Repository)
	repository = strings.TrimPrefix(repository, "https://github.com/")
	repository = strings.TrimPrefix(repository, "http://github.com/")
	repository = strings.TrimPrefix(repository, "ssh://git@github.com/")
	repository = strings.TrimPrefix(repository, "git@github.com:")
	repository = strings.TrimSuffix(strings.TrimSuffix(repository, "/"), ".git")
	if !githubRepositoryPattern.MatchString(repository) {
		return nil, errors.New("repository must be a GitHub owner/repo name or GitHub clone URL")
	}
	transport := strings.ToLower(strings.TrimSpace(raw.Transport))
	var cloneURL string
	switch transport {
	case "ssh":
		cloneURL = "git@github.com:" + repository + ".git"
	case "https":
		cloneURL = "https://github.com/" + repository + ".git"
	default:
		return nil, errors.New("clone transport must be ssh or https")
	}
	destination, err := resolveGroupDefaultCwd(raw.Destination)
	if err != nil {
		return nil, err
	}
	if destination == "" {
		return nil, errors.New("clone destination is required")
	}
	if destination == filepath.Dir(destination) {
		return nil, errors.New("clone destination must not be a filesystem root")
	}
	if _, err := os.Lstat(destination); err == nil {
		return nil, fmt.Errorf("clone destination already exists: %s", destination)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot inspect clone destination %s: %v", destination, err)
	}
	return &preparedGroupRepositoryClone{
		CloneURL:    cloneURL,
		WebURL:      "https://github.com/" + repository,
		Label:       repository,
		Destination: destination,
	}, nil
}

var runGroupRepositoryClone = func(ctx context.Context, cloneURL, destination string) ([]byte, error) {
	return exec.CommandContext(ctx, "git", "clone", "--", cloneURL, destination).CombinedOutput()
}

// cloneGroupRepository creates missing parent directories, then lets git clone
// create the final repository directory. A failed clone may leave that final
// directory behind (git's normal behaviour); the error names it so the human
// can inspect or remove partial data rather than agentd deleting it silently.
func cloneGroupRepository(plan *preparedGroupRepositoryClone) error {
	if plan == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(plan.Destination), 0o755); err != nil {
		return fmt.Errorf("create clone parent directory: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), groupRepositoryCloneTimeout)
	defer cancel()
	output, err := runGroupRepositoryClone(ctx, plan.CloneURL, plan.Destination)
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(output))
	if len(detail) > 4000 {
		detail = detail[len(detail)-4000:]
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("git clone timed out after %s", groupRepositoryCloneTimeout)
	}
	if detail == "" {
		return fmt.Errorf("git clone failed: %w", err)
	}
	return fmt.Errorf("git clone failed: %s", detail)
}
