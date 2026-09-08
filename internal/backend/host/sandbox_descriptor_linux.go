//go:build linux

package host

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
	"golang.org/x/sys/unix"
)

// sandboxDescriptorInvocation builds the sparse-root Linux native boundary.
// Mount and network policy compilation happens before this function. Its
// descriptors must come from the trusted host binding step, not public JSON.
// The returned argument file stays open until exec has inherited it. Neither
// namespace failure nor mount failure permits an unconfined fallback.
func sandboxDescriptorInvocation(wrapper string, child ProcessSpec, bindings *SandboxMountBindings, privateNetwork bool) (ProcessSpec, *os.File, error) {
	return sandboxLinuxInvocation(wrapper, child, bindings, privateNetwork, false)
}

func sandboxExecInvocation(wrapper string, child ProcessSpec, bindings *SandboxMountBindings, privateNetwork bool) (ProcessSpec, *os.File, error) {
	return sandboxLinuxInvocation(wrapper, child, bindings, privateNetwork, true)
}

func sandboxLinuxInvocation(wrapper string, child ProcessSpec, bindings *SandboxMountBindings, privateNetwork, direct bool) (ProcessSpec, *os.File, error) {
	if !filepath.IsAbs(wrapper) || !filepath.IsAbs(child.Executable) || !filepath.IsAbs(child.Directory) {
		return ProcessSpec{}, nil, fmt.Errorf("sandbox invocation requires absolute executable and working paths")
	}
	if bindings == nil || len(bindings.files) != len(bindings.pins) || len(child.ExtraFiles) != 0 {
		return ProcessSpec{}, nil, fmt.Errorf("sandbox invocation requires its complete retained descriptor set")
	}
	if !child.ExactEnvironment {
		return ProcessSpec{}, nil, fmt.Errorf("sandbox invocation requires an explicit child environment")
	}
	args := []string{"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--die-with-parent", "--new-session", "--cap-drop", "ALL"}
	if privateNetwork {
		args = append(args, "--unshare-net")
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")
	// Ancestors must be mounted before descendants, even when a trusted
	// provider root was appended after an authored narrower grant.
	type operation struct {
		path     string
		pin      int
		overlay  *sandboxOverlay
		priority int
	}
	var order []operation
	for index, pin := range bindings.pins {
		priority := 0
		if index >= len(bindings.pins)-bindings.providerCount {
			priority = 2 // The launch's exact resources survive equal-path denies.
		}
		order = append(order, operation{path: pin.Guest, pin: index, priority: priority})
	}
	for _, overlay := range bindings.overlays {
		order = append(order, operation{path: overlay.Path, overlay: &overlay, priority: 1})
	}
	sort.SliceStable(order, func(a, b int) bool {
		depth := func(path string) int {
			if path == "/" {
				return 0
			}
			return strings.Count(path, "/")
		}
		left, right := depth(order[a].path), depth(order[b].path)
		if left == right {
			return order[a].priority < order[b].priority
		}
		return left < right
	})
	activeHides := map[string]bool{}
	var hides []string
	for _, item := range order {
		if overlay := item.overlay; overlay != nil {
			switch overlay.Kind {
			case "deny":
				if overlay.Path != "/" {
					args = append(args, "--tmpfs", overlay.Path)
				}
				hides = append(hides, overlay.Path)
				activeHides[overlay.Path] = true
			case "tmpfs":
				if overlay.Size != "" {
					size, err := sandboxpolicy.ParseMemoryLimitBytes(overlay.Size)
					if err != nil {
						return ProcessSpec{}, nil, err
					}
					args = append(args, "--size", strconv.FormatUint(size, 10))
				}
				args = append(args, "--tmpfs", overlay.Path)
				activeHides[overlay.Path] = false
			default:
				return ProcessSpec{}, nil, fmt.Errorf("unknown sandbox overlay")
			}
			continue
		}
		index := item.pin
		pin := bindings.pins[index]
		flag := "--ro-bind-fd"
		if pin.Access == model.SandboxFilesystemWrite {
			flag = "--bind-fd"
		}
		fd := 3 + index
		if direct {
			fd = int(bindings.files[index].Fd())
		}
		args = append(args, flag, strconv.Itoa(fd), pin.Guest)
		activeHides[pin.Guest] = false
	}
	// Keep hides writable only while constructing narrower mountpoints. The
	// non-recursive remount preserves explicit writable children and scratch.
	for _, path := range hides {
		if activeHides[path] && path != "/" {
			args = append(args, "--remount-ro", path)
		}
	}
	args = append(args, "--remount-ro", "/")
	args = append(args, "--clearenv")
	for _, entry := range MergeEnvironment(nil, child.Env) {
		name, value, found := strings.Cut(entry, "=")
		if !found || name == "" {
			return ProcessSpec{}, nil, fmt.Errorf("sandbox environment requires named literal values")
		}
		args = append(args, "--setenv", name, value)
	}
	args = append(args, "--chdir", child.Directory)
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return ProcessSpec{}, nil, fmt.Errorf("sandbox invocation contains a NUL argument")
		}
	}
	encoded := strings.Join(args, "\x00") + "\x00"
	if len(encoded) > 4<<20 {
		return ProcessSpec{}, nil, fmt.Errorf("sandbox invocation exceeds 4 MiB")
	}
	// Keep the child's literal environment out of wrapper argv and
	// out of the wrapper's dynamic-loader environment. A sealed anonymous file
	// also prevents a writable temporary launch script from changing intent.
	fd, err := unix.MemfdCreate("tclaude-sandbox-arguments", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return ProcessSpec{}, nil, err
	}
	file := os.NewFile(uintptr(fd), "sandbox-arguments")
	ok := false
	defer func() {
		if !ok {
			_ = file.Close()
		}
	}()
	if _, err := file.WriteString(encoded); err != nil {
		return ProcessSpec{}, nil, err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return ProcessSpec{}, nil, err
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		return ProcessSpec{}, nil, err
	}
	wrapped := child
	wrapped.Executable = wrapper
	// bubblewrap's argument-file parser consumes options only; the native
	// command remains after the outer separator, exactly as supplied by host.
	argumentFD := 3 + len(bindings.files)
	if direct {
		argumentFD = int(file.Fd())
	}
	wrapped.Args = append([]string{"--args", strconv.Itoa(argumentFD), "--", child.Executable}, child.Args...)
	wrapped.ExtraFiles = append(bindings.Files(), file)
	wrapped.Env = nil
	// The guest cwd may not exist on the host. Only bubblewrap enters it after
	// installing the selected mount set.
	wrapped.Directory = "/"
	ok = true
	return wrapped, file, nil
}
