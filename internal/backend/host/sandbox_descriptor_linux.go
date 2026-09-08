//go:build linux

package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
	"golang.org/x/sys/unix"
)

// sandboxDescriptorInvocation builds the sparse-root Linux native boundary.
// Mount and network policy compilation happens before this function. Its
// descriptors must come from the trusted host binding step, not public JSON.
// The returned argument file stays open until exec has inherited it. Neither
// namespace failure nor mount failure permits an unconfined fallback.
func sandboxDescriptorInvocation(wrapper string, child ProcessSpec, bindings *SandboxMountBindings, privateNetwork bool) (ProcessSpec, *os.File, error) {
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
	for index, pin := range bindings.pins {
		flag := "--ro-bind-fd"
		if pin.Access == model.SandboxFilesystemWrite {
			flag = "--bind-fd"
		}
		args = append(args, flag, strconv.Itoa(3+index), pin.Guest)
	}
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
	wrapped.Args = append([]string{"--args", strconv.Itoa(3 + len(bindings.files)), "--", child.Executable}, child.Args...)
	wrapped.ExtraFiles = append(bindings.Files(), file)
	wrapped.Env = nil
	// The guest cwd may not exist on the host. Only bubblewrap enters it after
	// installing the selected mount set.
	wrapped.Directory = "/"
	ok = true
	return wrapped, file, nil
}
