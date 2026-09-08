//go:build linux || darwin

package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// SandboxProviderResource is supplied by a cohesive provider for resources it
// owns. It is never decoded from a profile, desired configuration or public API.
// Sources keep their host spelling in the guest: providers cannot use this seam
// to replace another resource through a remap.
type SandboxProviderResource struct {
	Path   string
	Access model.SandboxFilesystemAccess
}

// BindSandboxProviderResources admits exact provider resources beneath the
// private-state floor, without granting their parents. The ordinary authored
// binding path never gets this exception. Directory declarations must describe
// an attempt-owned subtree, never the backend's private root itself.
func (i *SandboxPathInspector) BindSandboxProviderResources(ctx context.Context, resources []SandboxProviderResource) (*SandboxMountBindings, error) {
	if len(resources) > 64 {
		return nil, fmt.Errorf("sandbox provider resource set exceeds 64 entries")
	}
	b, err := i.BindSandboxMounts(ctx, nil)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = b.Close()
		}
	}()
	seen := map[string]bool{}
	for _, resource := range resources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !filepath.IsAbs(resource.Path) || filepath.Clean(resource.Path) != resource.Path || (resource.Access != model.SandboxFilesystemRead && resource.Access != model.SandboxFilesystemWrite) {
			return nil, fmt.Errorf("invalid sandbox provider resource")
		}
		canonical, err := filepath.EvalSymlinks(resource.Path)
		if err != nil {
			return nil, err
		}
		if seen[canonical] {
			return nil, fmt.Errorf("duplicate sandbox provider resource")
		}
		seen[canonical] = true
		for _, root := range i.roots {
			rel, err := filepath.Rel(canonical, root.path)
			if err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
				return nil, fmt.Errorf("sandbox provider resource contains protected host root")
			}
		}
		info, err := os.Stat(canonical)
		if err != nil {
			return nil, err
		}
		kind := ""
		switch {
		case info.IsDir():
			kind = "directory"
		case info.Mode().IsRegular():
			kind = "file"
		case info.Mode()&os.ModeSocket != 0:
			kind = "socket"
		}
		if kind == "" {
			return nil, fmt.Errorf("unsupported sandbox provider resource kind")
		}
		file, err := openSandboxProviderResource(canonical, kind)
		if err != nil {
			return nil, err
		}
		b.files = append(b.files, file)
		same, err := sandboxProviderResourceMatches(file, canonical, kind, info)
		if err != nil || !same {
			return nil, fmt.Errorf("sandbox provider resource changed during preparation")
		}
		after, err := os.Stat(resource.Path)
		if err != nil || !os.SameFile(info, after) {
			return nil, fmt.Errorf("sandbox provider resource path changed during preparation")
		}
		for _, root := range i.roots {
			if os.SameFile(info, root.identity) {
				return nil, fmt.Errorf("sandbox provider resource aliases protected root")
			}
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, fmt.Errorf("sandbox provider resource identity unavailable")
		}
		b.pins = append(b.pins, SandboxMountPin{Source: canonical, Guest: resource.Path, Access: resource.Access, Kind: kind, Device: uint64(stat.Dev), Inode: uint64(stat.Ino)})
	}
	b.providerCount = len(b.pins)
	success = true
	return b, nil
}

func (i *SandboxPathInspector) reopenSandboxChildBindings(ctx context.Context, mounts, resources []SandboxMountPin) (*SandboxMountBindings, error) {
	authored, err := i.ReopenSandboxMounts(ctx, mounts)
	if err != nil {
		return nil, err
	}
	requested := make([]SandboxProviderResource, len(resources))
	for index, pin := range resources {
		requested[index] = SandboxProviderResource{Path: pin.Guest, Access: pin.Access}
	}
	owned, err := i.BindSandboxProviderResources(ctx, requested)
	if err != nil {
		_ = authored.Close()
		return nil, err
	}
	for index, pin := range resources {
		if owned.pins[index] != pin {
			_ = authored.Close()
			_ = owned.Close()
			return nil, fmt.Errorf("retained sandbox provider resource identity changed")
		}
	}
	authored.pins = append(authored.pins, owned.pins...)
	authored.files = append(authored.files, owned.files...)
	authored.providerCount = len(owned.pins)
	return authored, nil
}
