//go:build linux || darwin

package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// SandboxChildArtifact identifies a private, immutable launch input. It is
// retained with host preparation evidence; it is not an executable public DTO.
type SandboxChildArtifact struct {
	Path   string
	Digest string
}

const SandboxChildCommand = "__sandbox-child"

func (a SandboxChildArtifact) Invocation(bootstrapExecutable string) (ProcessSpec, error) {
	if !filepath.IsAbs(bootstrapExecutable) || !filepath.IsAbs(a.Path) || len(a.Digest) != sha256.Size*2 {
		return ProcessSpec{}, fmt.Errorf("sandbox child invocation requires exact prepared paths")
	}
	return ProcessSpec{Executable: bootstrapExecutable, Args: []string{SandboxChildCommand, a.Path, a.Digest}, Directory: "/", ExactEnvironment: true}, nil
}

type sandboxRootPin struct {
	Configured string
	Canonical  string
	Device     uint64
	Inode      uint64
}

type sandboxChildInput struct {
	Version           int
	Platform          string
	Wrapper           string
	Executable        string
	Arguments         []string
	Directory         string
	Environment       []string
	Mounts            []SandboxMountPin
	ProviderResources []SandboxMountPin `json:",omitempty"`
	ProtectedRoots    []sandboxRootPin
	InheritedRoot     bool `json:",omitempty"`
	PrivateNetwork    bool
	ControlPort       int              `json:",omitempty"`
	Overlays          []sandboxOverlay `json:",omitempty"`
}

// PrepareSandboxChild retains the exact host-owned command across a terminal
// server boundary. Sources are pinned now and reopened by identity in the
// child; neither changed policies nor changed filesystem objects become grants.
func (i *SandboxPathInspector) PrepareSandboxChild(directory, wrapper string, child ProcessSpec, bindings *SandboxMountBindings, privateNetwork bool) (SandboxChildArtifact, error) {
	if i == nil || len(i.roots) == 0 || bindings == nil || len(bindings.files) != len(bindings.pins) || !child.ExactEnvironment || len(child.ExtraFiles) != 0 {
		return SandboxChildArtifact{}, fmt.Errorf("sandbox child requires complete trusted preparation")
	}
	if !filepath.IsAbs(directory) || !filepath.IsAbs(wrapper) || !filepath.IsAbs(child.Executable) || !filepath.IsAbs(child.Directory) {
		return SandboxChildArtifact{}, fmt.Errorf("sandbox child requires absolute paths")
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return SandboxChildArtifact{}, fmt.Errorf("sandbox child artifact requires a private directory")
	}
	input := sandboxChildInput{Version: 1, Platform: runtime.GOOS, Wrapper: wrapper, Executable: child.Executable,
		Arguments: child.Args, Directory: child.Directory, Environment: child.Env, Mounts: bindings.Pins()[:len(bindings.pins)-bindings.providerCount], ProviderResources: bindings.Pins()[len(bindings.pins)-bindings.providerCount:], PrivateNetwork: privateNetwork, ControlPort: bindings.controlPort}
	input.InheritedRoot = bindings.inheritedRoot
	input.Overlays = append([]sandboxOverlay(nil), bindings.overlays...)
	for _, root := range i.roots {
		stat, ok := root.identity.Sys().(*syscall.Stat_t)
		if !ok {
			return SandboxChildArtifact{}, fmt.Errorf("protected sandbox root identity is unavailable")
		}
		input.ProtectedRoots = append(input.ProtectedRoots, sandboxRootPin{Configured: root.configuredPath, Canonical: root.path, Device: uint64(stat.Dev), Inode: uint64(stat.Ino)})
	}
	if err := validateSandboxControlArguments(child.Args, bindings.controlPort); err != nil {
		return SandboxChildArtifact{}, err
	}
	text := append([]string{input.Wrapper, input.Executable, input.Directory}, input.Arguments...)
	text = append(text, input.Environment...)
	for _, root := range input.ProtectedRoots {
		text = append(text, root.Configured, root.Canonical)
	}
	for _, pin := range bindings.pins {
		text = append(text, pin.Source, pin.Guest)
	}
	for _, value := range text {
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return SandboxChildArtifact{}, fmt.Errorf("sandbox child intent contains invalid text")
		}
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return SandboxChildArtifact{}, err
	}
	if len(encoded) > 4<<20 {
		return SandboxChildArtifact{}, fmt.Errorf("sandbox child artifact exceeds 4 MiB")
	}
	path := filepath.Join(directory, "sandbox-child.json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return SandboxChildArtifact{}, err
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(encoded); err != nil {
		return SandboxChildArtifact{}, err
	}
	if err := file.Sync(); err != nil {
		return SandboxChildArtifact{}, err
	}
	digest := sha256.Sum256(encoded)
	ok = true
	return SandboxChildArtifact{Path: path, Digest: hex.EncodeToString(digest[:])}, nil
}

// ExecuteSandboxChild replaces only the isolated bootstrap process. Production
// entrypoints call it before starting application goroutines or opening state.
// It never talks to the daemon or consumes/replays a public operation.
func ExecuteSandboxChild(ctx context.Context, artifact SandboxChildArtifact) error {
	input, inspector, err := readSandboxChild(artifact)
	if err != nil {
		return err
	}
	bound, err := inspector.reopenSandboxChildBindings(ctx, input.Mounts, input.ProviderResources)
	if err != nil {
		return err
	}
	defer func() { _ = bound.Close() }()
	if err := inspector.reopenSandboxOverlays(ctx, bound, input.Overlays, input.Executable, input.Directory); err != nil {
		return err
	}
	if err := inspector.setSandboxRoot(bound, input.InheritedRoot, input.PrivateNetwork); err != nil {
		return err
	}
	bound.controlPort = input.ControlPort
	wrapped, arguments, err := sandboxExecInvocation(input.Wrapper, ProcessSpec{Executable: input.Executable, Args: input.Arguments,
		Directory: input.Directory, Env: input.Environment, ExactEnvironment: true}, bound, input.PrivateNetwork)
	if err != nil {
		return err
	}
	if arguments != nil {
		defer func() { _ = arguments.Close() }()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// The terminal server must never respawn a retained command artifact. A
	// failed/uncertain exec remains consumed and must be observed, not replayed.
	marker, err := os.OpenFile(artifact.Path+".started", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("sandbox child launch was already attempted or cannot be claimed: %w", err)
	}
	if err := marker.Sync(); err != nil {
		_ = marker.Close()
		return err
	}
	if err := marker.Close(); err != nil {
		return err
	}
	if input.ControlPort != 0 {
		file, err := createSandboxControl(artifact)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		for index, value := range wrapped.Args {
			if value == SandboxControlFDArgument {
				wrapped.Args[index] = fmt.Sprint(file.Fd())
			}
		}
		wrapped.ExtraFiles = append(wrapped.ExtraFiles, file)
	}
	if err := os.Chdir(wrapped.Directory); err != nil {
		return err
	}
	// Preserve the bootstrap PID and terminal process-group identity. Use the
	// descriptors' actual numbers instead of overwriting the Go runtime's FDs.
	for _, file := range wrapped.ExtraFiles {
		if _, err := unix.FcntlInt(file.Fd(), unix.F_SETFD, 0); err != nil {
			return err
		}
	}
	return syscall.Exec(wrapped.Executable, append([]string{wrapped.Executable}, wrapped.Args...), wrapped.Env)
}

// VerifySandboxChild checks retained host identities before release admission.
// The child repeats this check and acquires descriptors immediately before exec.
func VerifySandboxChild(ctx context.Context, artifact SandboxChildArtifact) error {
	input, inspector, err := readSandboxChild(artifact)
	if err != nil {
		return err
	}
	bound, err := inspector.reopenSandboxChildBindings(ctx, input.Mounts, input.ProviderResources)
	if err != nil {
		return err
	}
	defer func() { _ = bound.Close() }()
	if err := inspector.reopenSandboxOverlays(ctx, bound, input.Overlays, input.Executable, input.Directory); err != nil {
		return err
	}
	return inspector.setSandboxRoot(bound, input.InheritedRoot, input.PrivateNetwork)
}

func readSandboxChild(artifact SandboxChildArtifact) (sandboxChildInput, *SandboxPathInspector, error) {
	var input sandboxChildInput
	if !filepath.IsAbs(artifact.Path) || len(artifact.Digest) != sha256.Size*2 {
		return input, nil, fmt.Errorf("invalid sandbox child artifact identity")
	}
	fd, err := syscall.Open(artifact.Path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return input, nil, err
	}
	file := os.NewFile(uintptr(fd), artifact.Path)
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4<<20 {
		return input, nil, fmt.Errorf("sandbox child artifact is not a bounded private regular file")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	if err != nil {
		return input, nil, err
	}
	digest := sha256.Sum256(encoded)
	if len(encoded) > 4<<20 || hex.EncodeToString(digest[:]) != artifact.Digest {
		return input, nil, fmt.Errorf("sandbox child artifact identity changed")
	}
	if err := json.Unmarshal(encoded, &input); err != nil {
		return input, nil, err
	}
	if input.Version != 1 || input.Platform != runtime.GOOS || len(input.ProtectedRoots) == 0 || len(input.ProtectedRoots) > 128 {
		return input, nil, fmt.Errorf("unsupported sandbox child artifact")
	}
	if err := validateSandboxControlArguments(input.Arguments, input.ControlPort); err != nil {
		return input, nil, err
	}
	roots := make([]string, len(input.ProtectedRoots))
	for index, root := range input.ProtectedRoots {
		roots[index] = root.Configured
	}
	inspector, err := NewSandboxPathInspector(roots)
	if err != nil {
		return input, nil, err
	}
	for index, root := range inspector.roots {
		stat, ok := root.identity.Sys().(*syscall.Stat_t)
		pin := input.ProtectedRoots[index]
		if !ok || root.path != pin.Canonical || uint64(stat.Dev) != pin.Device || uint64(stat.Ino) != pin.Inode {
			return input, nil, fmt.Errorf("protected sandbox root identity changed")
		}
	}
	return input, inspector, nil
}
