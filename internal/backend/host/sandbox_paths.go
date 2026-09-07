//go:build linux || darwin

package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

type sandboxProtectedRoot struct {
	path     string
	identity os.FileInfo
}

type SandboxPathInspector struct{ roots []sandboxProtectedRoot }

// NewSandboxPathInspector takes composition-owned protected directories. It
// does not infer a home directory or daemon layout from ambient process state.
// The list cannot be replaced by a user-authored policy or public request.
func NewSandboxPathInspector(protectedRoots []string) (*SandboxPathInspector, error) {
	if len(protectedRoots) == 0 || len(protectedRoots) > 128 {
		return nil, fmt.Errorf("sandbox inspection requires 1..128 explicit protected roots")
	}
	inspector := &SandboxPathInspector{}
	for _, root := range protectedRoots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return nil, fmt.Errorf("protected sandbox root must be a clean absolute path")
		}
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("resolve protected sandbox root: %w", err)
		}
		info, err := os.Stat(canonical)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("protected sandbox root must be a directory")
		}
		inspector.roots = append(inspector.roots, sandboxProtectedRoot{canonical, info})
	}
	return inspector, nil
}

func (i *SandboxPathInspector) InspectSandboxPaths(ctx context.Context, rules []model.SandboxFilesystemRule) ([]ports.SandboxPathObservation, error) {
	if i == nil || len(i.roots) == 0 {
		return nil, fmt.Errorf("sandbox path inspector is not configured")
	}
	if err := sandboxpolicy.Validate(model.SandboxPolicy{Filesystem: rules}); err != nil {
		return nil, err
	}
	for _, root := range i.roots {
		current, err := os.Stat(root.path)
		if err != nil {
			return nil, fmt.Errorf("protected sandbox root is unavailable: %w", err)
		}
		if !os.SameFile(current, root.identity) {
			return nil, fmt.Errorf("protected sandbox root identity changed")
		}
	}
	out := make([]ports.SandboxPathObservation, 0, len(rules))
	for index, rule := range rules {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		observation := ports.SandboxPathObservation{Index: index, State: "unknown"}
		canonical, err := filepath.EvalSymlinks(rule.HostPath)
		if err != nil {
			if os.IsNotExist(err) {
				observation.State = "missing"
				observation.Detail = "Host path does not exist; no directory was created."
			} else {
				observation.Detail = "Host path could not be resolved."
			}
			out = append(out, observation)
			continue
		}
		observation.CanonicalPath = canonical
		info, err := os.Stat(canonical)
		if err != nil {
			observation.Detail = "Host path changed or became unavailable during inspection."
			out = append(out, observation)
			continue
		}
		switch {
		case info.IsDir():
			observation.Kind = "directory"
		case info.Mode().IsRegular():
			observation.Kind = "file"
		default:
			observation.State = "refused"
			observation.Detail = "Filesystem grants require a regular file or directory."
		}
		if observation.State == "refused" {
			out = append(out, observation)
			continue
		}
		if rule.ExpectedKind != "" && rule.ExpectedKind != observation.Kind {
			observation.State = "refused"
			observation.Detail = "Observed path kind differs from the authored commitment."
		} else if rule.Access == model.SandboxFilesystemDeny && !info.IsDir() {
			observation.State = "refused"
			observation.Detail = "A deny rule must name a directory."
		} else if rule.Access != model.SandboxFilesystemDeny {
			for _, root := range i.roots {
				// Conservative folded spelling guards preserve the legacy protection for
				// case aliases. Identity ancestry additionally covers existing aliases on
				// case-insensitive/normalizing filesystems without guessing their spelling.
				if rule.GuestPath != "" && sandboxPathSpellingIntersects(rule.GuestPath, root.path) {
					observation.State = "refused"
					observation.Detail = "Guest mount path intersects protected host state."
					break
				}
				intersects, checkErr := sandboxPathsIntersect(canonical, info, root)
				if checkErr != nil {
					observation.State = "unknown"
					observation.Detail = "Host ancestry could not be inspected."
					break
				}
				if intersects {
					observation.State = "refused"
					observation.Detail = "Grant intersects protected host state."
					break
				}
				observation.State = "available"
			}
		} else {
			observation.State = "available"
		}
		out = append(out, observation)
	}
	return out, nil
}

func sandboxPathsIntersect(canonical string, info os.FileInfo, root sandboxProtectedRoot) (bool, error) {
	if sandboxPathSpellingIntersects(canonical, root.path) {
		return true, nil
	}
	// The leaf may be a file; its parent chain still establishes whether the
	// protected directory is an ancestor. The reverse catches directory grants
	// above protected state even through a platform-specific spelling alias.
	for _, check := range []struct {
		start  string
		target os.FileInfo
	}{{canonical, root.identity}, {root.path, info}} {
		for current := check.start; ; current = filepath.Dir(current) {
			observed, err := os.Stat(current)
			if err != nil {
				return false, err
			}
			if os.SameFile(observed, check.target) {
				return true, nil
			}
			if filepath.Dir(current) == current {
				break
			}
		}
	}
	return false, nil
}

func sandboxPathSpellingIntersects(a, b string) bool {
	left, right := strings.ToLower(filepath.Clean(a)), strings.ToLower(filepath.Clean(b))
	within := func(child, parent string) bool {
		return child == parent || strings.HasPrefix(child, strings.TrimRight(parent, string(filepath.Separator))+string(filepath.Separator))
	}
	return within(left, right) || within(right, left)
}
