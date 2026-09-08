//go:build linux || darwin

package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

// Overlays convey no host read authority and own no source descriptor. Denies
// retain their original source spelling so a changed alias cannot redirect the
// restriction. Tmpfs paths name the guest namespace exclusively.
type sandboxOverlay struct {
	Kind     string
	Path     string
	HostPath string `json:",omitempty"`
	Size     string `json:",omitempty"`
}

func (i *SandboxPathInspector) prepareSandboxOverlays(ctx context.Context, denied []model.SandboxFilesystemRule, temporary []model.SandboxTmpfs, bound *SandboxMountBindings, executable, directory string) ([]sandboxOverlay, error) {
	if err := sandboxpolicy.Validate(model.SandboxPolicy{Filesystem: denied, Tmpfs: temporary}); err != nil {
		return nil, err
	}
	if len(temporary) != 0 && runtime.GOOS == "darwin" {
		return nil, fmt.Errorf("macOS Seatbelt cannot create temporary filesystem mounts; tmpfs requires Linux mount namespaces")
	}
	observed, err := i.InspectSandboxPaths(ctx, denied)
	if err != nil {
		return nil, err
	}
	var overlays []sandboxOverlay
	for index, rule := range denied {
		row := observed[index]
		if rule.Access != model.SandboxFilesystemDeny || row.CanonicalPath == "" || (row.State != "available" && row.State != "missing") {
			return nil, fmt.Errorf("sandbox deny %d cannot be resolved: %s", index, row.Detail)
		}
		overlays = append(overlays, sandboxOverlay{Kind: "deny", Path: row.CanonicalPath, HostPath: rule.HostPath})
	}
	for _, mount := range temporary {
		protected, err := i.guestPathProtected(ctx, mount.GuestPath)
		if err != nil || protected {
			return nil, fmt.Errorf("temporary filesystem intersects protected host state")
		}
		if info, err := os.Stat(mount.GuestPath); err == nil && !info.IsDir() {
			return nil, fmt.Errorf("temporary filesystem mount point must be a directory")
		} else if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		canonical, _, err := resolveMissingSandboxPath(ctx, mount.GuestPath, 0)
		if err != nil {
			return nil, err
		}
		required := []string{executable, directory}
		for _, pin := range bound.pins[len(bound.pins)-bound.providerCount:] {
			required = append(required, pin.Guest)
		}
		for _, path := range required {
			resolved, _ := filepath.EvalSymlinks(path)
			if sandboxPathSpellingContains(mount.GuestPath, path) || sandboxPathSpellingContains(canonical, path) ||
				(resolved != "" && (sandboxPathSpellingContains(mount.GuestPath, resolved) || sandboxPathSpellingContains(canonical, resolved))) {
				return nil, fmt.Errorf("temporary filesystem at %s would shadow launch-required path %s", mount.GuestPath, path)
			}
		}
		overlays = append(overlays, sandboxOverlay{Kind: "tmpfs", Path: mount.GuestPath, Size: mount.Size})
	}
	return overlays, nil
}

func (i *SandboxPathInspector) reopenSandboxOverlays(ctx context.Context, bound *SandboxMountBindings, retained []sandboxOverlay, executable, directory string) error {
	if len(retained) > 640 {
		return fmt.Errorf("retained sandbox overlays exceed the policy limit")
	}
	var denied []model.SandboxFilesystemRule
	var temporary []model.SandboxTmpfs
	for _, overlay := range retained {
		switch overlay.Kind {
		case "deny":
			denied = append(denied, model.SandboxFilesystemRule{HostPath: overlay.HostPath, Access: model.SandboxFilesystemDeny})
		case "tmpfs":
			temporary = append(temporary, model.SandboxTmpfs{GuestPath: overlay.Path, Size: overlay.Size})
		default:
			return fmt.Errorf("unknown retained sandbox overlay")
		}
	}
	current, err := i.prepareSandboxOverlays(ctx, denied, temporary, bound, executable, directory)
	if err != nil {
		return err
	}
	if !slices.Equal(retained, current) {
		return fmt.Errorf("retained sandbox overlay path changed")
	}
	bound.overlays = current
	return nil
}

func sandboxPathSpellingContains(parent, child string) bool {
	parent, child = strings.ToLower(filepath.Clean(parent)), strings.ToLower(filepath.Clean(child))
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
