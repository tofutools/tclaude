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
// Sources keep their host spelling in the guest, except the narrowly derived
// OpenCode private config projection constructed below.
type SandboxProviderResource struct {
	Path                string
	Access              model.SandboxFilesystemAccess
	openCodeConfigState string
	rejectAliases       bool
	preserveDirectory   bool
}

// SandboxOpenCodeConfiguration projects a trusted native config directory only
// onto config/opencode beneath a separately declared writable provider state.
// Both source and destination identities are retained and rechecked at release.
func SandboxOpenCodeConfiguration(source, state string, access model.SandboxFilesystemAccess) SandboxProviderResource {
	return SandboxProviderResource{Path: source, Access: access, openCodeConfigState: state}
}

// BindSandboxProviderResources admits exact provider resources beneath the
// private-state floor, without granting their parents. The ordinary authored
// binding path never gets this exception. Directory declarations must describe
// an attempt-owned subtree, never the backend's private root itself.
func (i *SandboxPathInspector) BindSandboxProviderResources(ctx context.Context, resources []SandboxProviderResource) (*SandboxMountBindings, error) {
	// Includes up to 128 individually mounted generated directories plus
	// bounded provider configuration and control resources.
	if len(resources) > 192 {
		return nil, fmt.Errorf("sandbox provider resource set exceeds 192 entries")
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
	seenGuests := map[string]bool{}
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
		if resource.rejectAliases && canonical != resource.Path {
			return nil, fmt.Errorf("generated sandbox directory alias is not permitted")
		}
		guest := resource.Path
		identityKey := canonical
		var targetDevice, targetInode uint64
		if resource.openCodeConfigState != "" {
			guest, targetDevice, targetInode, err = i.openCodeConfigurationTarget(resource.openCodeConfigState, resources)
			if err != nil {
				return nil, err
			}
			identityKey += "\x00" + guest
		}
		if seen[identityKey] || seenGuests[guest] {
			return nil, fmt.Errorf("duplicate sandbox provider resource")
		}
		seen[identityKey], seenGuests[guest] = true, true
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
		if resource.preserveDirectory && kind != "directory" {
			return nil, fmt.Errorf("retained generated directory must remain a directory")
		}
		if kind == "" {
			return nil, fmt.Errorf("unsupported sandbox provider resource kind")
		}
		if resource.openCodeConfigState != "" && kind != "directory" {
			return nil, fmt.Errorf("OpenCode configuration source must be a directory")
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
		b.pins = append(b.pins, SandboxMountPin{Source: canonical, Guest: guest, Access: resource.Access, Kind: kind, Device: uint64(stat.Dev), Inode: uint64(stat.Ino), OpenCodeConfigState: resource.openCodeConfigState, ConfigTargetDevice: targetDevice, ConfigTargetInode: targetInode, PreserveDirectory: resource.preserveDirectory})
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
		requested[index] = SandboxProviderResource{Path: pin.Guest, Access: pin.Access, preserveDirectory: pin.PreserveDirectory}
		if pin.OpenCodeConfigState != "" {
			requested[index] = SandboxOpenCodeConfiguration(pin.Source, pin.OpenCodeConfigState, pin.Access)
		}
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

func (i *SandboxPathInspector) openCodeConfigurationTarget(state string, resources []SandboxProviderResource) (string, uint64, uint64, error) {
	if !filepath.IsAbs(state) || filepath.Clean(state) != state {
		return "", 0, 0, fmt.Errorf("OpenCode configuration requires an absolute private state root")
	}
	declared := false
	for _, resource := range resources {
		if resource.Path == state && resource.Access == model.SandboxFilesystemWrite && resource.openCodeConfigState == "" {
			declared = true
		}
	}
	protected := false
	for _, root := range i.roots {
		rel, err := filepath.Rel(root.path, state)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			protected = true
		}
	}
	if !declared || !protected {
		return "", 0, 0, fmt.Errorf("OpenCode configuration target requires declared private writable state")
	}
	target := filepath.Join(state, "config", "opencode")
	canonical, err := filepath.EvalSymlinks(target)
	if err != nil || canonical != target {
		return "", 0, 0, fmt.Errorf("OpenCode private configuration target changed")
	}
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		return "", 0, 0, fmt.Errorf("OpenCode private configuration target must be a directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", 0, 0, fmt.Errorf("OpenCode configuration target identity unavailable")
	}
	return target, uint64(stat.Dev), uint64(stat.Ino), nil
}

// SandboxControlResource uses a composition-owned socket namespace when one is
// supplied. Mounting its readonly directory lets a recreated socket remain
// reachable after daemon restart; the directory must contain no other state.
// Exact socket mode remains available for independently owned fixed endpoints.
func SandboxControlResource(socket, directory string) (SandboxProviderResource, error) {
	if directory == "" {
		return SandboxProviderResource{Path: socket, Access: model.SandboxFilesystemRead}, nil
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || filepath.Dir(socket) != directory {
		return SandboxProviderResource{}, fmt.Errorf("sandbox control directory must own the exact socket")
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return SandboxProviderResource{}, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return SandboxProviderResource{}, fmt.Errorf("sandbox control directory must be a private directory")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return SandboxProviderResource{}, err
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(socket) {
			return SandboxProviderResource{}, fmt.Errorf("sandbox control directory contains unrelated state")
		}
	}
	info, err = os.Lstat(socket)
	if err != nil {
		return SandboxProviderResource{}, err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return SandboxProviderResource{}, fmt.Errorf("sandbox control endpoint is not a socket")
	}
	return SandboxProviderResource{Path: directory, Access: model.SandboxFilesystemRead}, nil
}
