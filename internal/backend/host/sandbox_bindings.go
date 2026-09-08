//go:build linux || darwin

package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// SandboxMountPin is host evidence for one positive filesystem grant. It must
// be retained inside the host-owned launch artifact, never accepted as public
// authority. Reopening verifies identity as well as the authored access/kind.
type SandboxMountPin struct {
	Source string
	Guest  string
	Access model.SandboxFilesystemAccess
	Kind   string
	Device uint64
	Inode  uint64
	// OpenCodeConfigState identifies the sole provider-owned config projection.
	// The destination is always its config/opencode directory, never an
	// arbitrary provider-supplied guest path.
	OpenCodeConfigState string `json:",omitempty"`
	ConfigTargetDevice  uint64 `json:",omitempty"`
	ConfigTargetInode   uint64 `json:",omitempty"`
	// An individually granted generated directory may be written within,
	// but its own directory entry must remain in place.
	PreserveDirectory bool `json:",omitempty"`
}

// SandboxMountBindings owns the descriptors backing a prepared mount set.
// Close it after the native wrapper has inherited the descriptors, or on abort.
// Deny regions and temporary mounts are compiled separately: neither grants a
// host source descriptor. A binding alone is not an OS confinement receipt.
type SandboxMountBindings struct {
	resources               *sandboxCgroup
	providerCount           int
	inheritedRoot           bool
	protectedRoots          []string
	controlPort             int
	darwinAllowMachRegister bool
	overlays                []sandboxOverlay
	pins                    []SandboxMountPin
	files                   []*os.File
}

func (b *SandboxMountBindings) Pins() []SandboxMountPin {
	return append([]SandboxMountPin(nil), b.pins...)
}

// Files returns borrowed descriptors, in Pins order, for exec.Cmd.ExtraFiles.
// The caller must not close them or use their offsets for ordinary file IO.
func (b *SandboxMountBindings) Files() []*os.File {
	return append([]*os.File(nil), b.files...)
}

func (b *SandboxMountBindings) Close() error {
	var first error
	for _, file := range b.files {
		if err := file.Close(); err != nil && first == nil {
			first = err
		}
	}
	b.files = nil
	return first
}

// BindSandboxMounts turns inspected positive grants into retained native file
// identities. It creates no source paths and follows no final-component links
// after inspection. Missing paths and changing ancestry fail before launch.
func (i *SandboxPathInspector) BindSandboxMounts(ctx context.Context, rules []model.SandboxFilesystemRule) (*SandboxMountBindings, error) {
	observations, err := i.InspectSandboxPaths(ctx, rules)
	if err != nil {
		return nil, err
	}
	b := &SandboxMountBindings{}
	ok := false
	defer func() {
		if !ok {
			_ = b.Close()
		}
	}()
	for index, rule := range rules {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if rule.Access != model.SandboxFilesystemRead && rule.Access != model.SandboxFilesystemWrite {
			return nil, fmt.Errorf("sandbox source bindings require positive filesystem grants")
		}
		observation := observations[index]
		if observation.State != "available" {
			return nil, fmt.Errorf("sandbox source %d is %s: %s", index, observation.State, observation.Detail)
		}
		// NONBLOCK prevents a concurrently substituted FIFO/device from hanging
		// preparation; fstat below admits only ordinary files and directories.
		fd, err := syscall.Open(observation.CanonicalPath, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, fmt.Errorf("open sandbox source %d: %w", index, err)
		}
		file := os.NewFile(uintptr(fd), observation.CanonicalPath)
		b.files = append(b.files, file)
		info, err := file.Stat()
		if err != nil {
			return nil, err
		}
		kind := ""
		if info.IsDir() {
			kind = "directory"
		} else if info.Mode().IsRegular() {
			kind = "file"
		}
		if kind == "" || kind != observation.Kind {
			return nil, fmt.Errorf("sandbox source %d changed kind during preparation", index)
		}
		// Reinspect after opening. This rechecks protected-root identity and both
		// source/guest ancestry, including configured and canonical aliases.
		current, err := i.InspectSandboxPaths(ctx, []model.SandboxFilesystemRule{rule})
		if err != nil {
			return nil, err
		}
		if current[0].State != "available" || current[0].CanonicalPath != observation.CanonicalPath {
			return nil, fmt.Errorf("sandbox source %d changed during preparation", index)
		}
		currentInfo, err := os.Stat(observation.CanonicalPath)
		if err != nil || !os.SameFile(info, currentInfo) {
			return nil, fmt.Errorf("sandbox source %d identity changed during preparation", index)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, fmt.Errorf("sandbox source identity is unavailable")
		}
		guest := rule.GuestPath
		if guest == "" {
			guest = observation.CanonicalPath
		}
		b.pins = append(b.pins, SandboxMountPin{Source: observation.CanonicalPath, Guest: guest,
			Access: rule.Access, Kind: kind, Device: uint64(stat.Dev), Inode: uint64(stat.Ino)})
	}
	ok = true
	return b, nil
}

// ReopenSandboxMounts is used by the trusted native child bootstrap after a
// terminal server boundary. It never interprets changed paths as new grants.
func (i *SandboxPathInspector) ReopenSandboxMounts(ctx context.Context, pins []SandboxMountPin) (*SandboxMountBindings, error) {
	if len(pins) > 512 {
		return nil, fmt.Errorf("retained sandbox source set exceeds 512 mounts")
	}
	rules := make([]model.SandboxFilesystemRule, len(pins))
	for index, pin := range pins {
		if !filepath.IsAbs(pin.Source) || filepath.Clean(pin.Source) != pin.Source || pin.Kind != "file" && pin.Kind != "directory" {
			return nil, fmt.Errorf("invalid retained sandbox source")
		}
		rules[index] = model.SandboxFilesystemRule{HostPath: pin.Source, GuestPath: pin.Guest, Access: pin.Access, ExpectedKind: pin.Kind}
	}
	bound, err := i.BindSandboxMounts(ctx, rules)
	if err != nil {
		return nil, err
	}
	for index, pin := range pins {
		if bound.pins[index] != pin {
			_ = bound.Close()
			return nil, fmt.Errorf("retained sandbox source %d identity changed", index)
		}
	}
	return bound, nil
}
