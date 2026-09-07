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
	path           string
	configuredPath string
	identity       os.FileInfo
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
		inspector.roots = append(inspector.roots, sandboxProtectedRoot{path: canonical, configuredPath: root, identity: info})
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
		if rule.Access != model.SandboxFilesystemDeny && rule.GuestPath != "" {
			refused, checkErr := i.guestPathProtected(ctx, rule.GuestPath)
			if checkErr != nil || refused {
				if checkErr != nil {
					observation.Detail = "Guest path ancestry could not be inspected."
				} else {
					observation.State = "refused"
					observation.Detail = "Guest mount path intersects protected host state."
				}
				out = append(out, observation)
				continue
			}
		}
		canonical, err := filepath.EvalSymlinks(rule.HostPath)
		if err != nil {
			if os.IsNotExist(err) {
				projected, ancestor, resolveErr := resolveMissingSandboxPath(ctx, rule.HostPath, 0)
				if resolveErr != nil {
					observation.Detail = "Missing path ancestry could not be resolved."
				} else {
					observation.CanonicalPath = projected
					observation.State = "missing"
					observation.Detail = "Host path does not exist; no directory was created."
					if rule.Access != model.SandboxFilesystemDeny {
						refused, checkErr := i.missingPathProtected(projected, ancestor)
						if checkErr != nil {
							observation.State = "unknown"
							observation.Detail = "Missing path ancestry could not be inspected."
						} else if refused {
							observation.State = "refused"
							observation.Detail = "Missing grant intersects protected host state."
						}
					}
				}
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
				if rule.GuestPath != "" && (sandboxPathSpellingIntersects(rule.GuestPath, root.path) || sandboxPathSpellingIntersects(rule.GuestPath, root.configuredPath)) {
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

func (i *SandboxPathInspector) protectedSpelling(path string) bool {
	if path == "" {
		return false
	}
	for _, root := range i.roots {
		if sandboxPathSpellingIntersects(path, root.path) || sandboxPathSpellingIntersects(path, root.configuredPath) {
			return true
		}
	}
	return false
}

// Resolve only existing ancestors. A dangling symlink must still contribute its
// target spelling; stripping it as if it were an ordinary absent child would
// lose a private target. No directory is created and symlink recursion is bounded.
func resolveMissingSandboxPath(ctx context.Context, path string, links int) (string, string, error) {
	if links > 40 {
		return "", "", fmt.Errorf("too many symlinks")
	}
	candidate := path
	suffix := []string{}
	for {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		canonical, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			parts := append([]string{canonical}, suffix...)
			return filepath.Join(parts...), canonical, nil
		}
		if !os.IsNotExist(err) {
			return "", "", err
		}
		info, statErr := os.Lstat(candidate)
		if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(candidate)
			if err != nil {
				return "", "", err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(candidate), target)
			}
			return resolveMissingSandboxPath(ctx, filepath.Join(append([]string{target}, suffix...)...), links+1)
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", "", err
		}
		suffix = append([]string{filepath.Base(candidate)}, suffix...)
		candidate = parent
	}
}
func (i *SandboxPathInspector) missingPathProtected(projected, ancestor string) (bool, error) {
	if i.protectedSpelling(projected) {
		return true, nil
	}
	// Only test whether the existing ancestor is within private state. Testing
	// the reverse would incorrectly refuse an ordinary missing sibling merely
	// because its existing parent also contains the private directory.
	for current := ancestor; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err != nil {
			return false, err
		}
		for _, root := range i.roots {
			if os.SameFile(info, root.identity) {
				return true, nil
			}
		}
		if filepath.Dir(current) == current {
			return false, nil
		}
	}
}

// Guest paths remain authored namespace paths. Host resolution is used only
// for conservative protected-state comparisons, never to rewrite the mount.
func (i *SandboxPathInspector) guestPathProtected(ctx context.Context, path string) (bool, error) {
	if i.protectedSpelling(path) {
		return true, nil
	}
	projected, ancestor, err := resolveMissingSandboxPath(ctx, path, 0)
	if err != nil {
		return false, err
	}
	return i.missingPathProtected(projected, ancestor)
}
