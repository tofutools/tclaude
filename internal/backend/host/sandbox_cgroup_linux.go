//go:build linux

package host

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
	"golang.org/x/sys/unix"
)

func sandboxResourceDelegation(configured string) (string, error) {
	if configured != "" {
		if !filepath.IsAbs(configured) {
			return "", fmt.Errorf("resource delegation directory must be absolute")
		}
		return filepath.Clean(configured), nil
	}
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			dir := filepath.Join("/sys/fs/cgroup", filepath.Clean("/"+path))
			if filepath.Base(dir) == "tclaude-supervisor" {
				dir = filepath.Dir(dir)
			}
			return dir, nil
		}
	}
	return "", fmt.Errorf("resource limits require a delegated cgroup v2 subtree")
}

func prepareSandboxCgroup(delegation string, limits model.SandboxResources) (*sandboxCgroup, error) {
	if limits == (model.SandboxResources{}) {
		return nil, nil
	}
	memory, cpu := "", ""
	needed := []string{}
	if limits.Memory != "" {
		value, err := sandboxpolicy.ParseMemoryLimitBytes(limits.Memory)
		if err != nil {
			return nil, err
		}
		memory = strconv.FormatUint(value, 10)
		needed = append(needed, "memory")
	}
	if limits.CPU != "" {
		value, err := sandboxpolicy.CPUQuotaMicros(limits.CPU)
		if err != nil {
			return nil, err
		}
		cpu = fmt.Sprintf("%d 100000", value)
		needed = append(needed, "cpu")
	}
	root, err := sandboxResourceDelegation(delegation)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resource delegation unavailable: %w", err)
	}
	file, err := openSandboxCgroup(canonical)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	controllers, err := readCgroupFile(file, "cgroup.controllers")
	if err != nil {
		return nil, err
	}
	enabled, err := readCgroupFile(file, "cgroup.subtree_control")
	if err != nil {
		return nil, err
	}
	var enable []string
	for _, name := range needed {
		if !slices.Contains(strings.Fields(string(controllers)), name) {
			return nil, fmt.Errorf("resource limits require the delegated cgroup v2 %s controller; configure Delegate=cpu memory and DelegateSubgroup=tclaude-supervisor", name)
		}
		if !slices.Contains(strings.Fields(string(enabled)), name) {
			enable = append(enable, "+"+name)
		}
	}
	if len(enable) > 0 {
		if err := writeCgroupFile(file, "cgroup.subtree_control", strings.Join(enable, " ")); err != nil {
			return nil, fmt.Errorf("enable resource controllers in %s: %w; use a process-free delegated subtree (Delegate=cpu memory, DelegateSubgroup=tclaude-supervisor)", canonical, err)
		}
	}
	token, err := randomToken(12)
	if err != nil {
		return nil, err
	}
	name := "tclaude-launch-" + token
	if err := unix.Mkdirat(int(file.Fd()), name, 0755); err != nil {
		return nil, fmt.Errorf("create resource boundary in %s: %w; configure a writable delegated cgroup subtree", canonical, err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = unix.Unlinkat(int(file.Fd()), name, unix.AT_REMOVEDIR)
		}
	}()
	fd, err := unix.Openat(int(file.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(canonical, name)
	boundary := os.NewFile(uintptr(fd), dir)
	defer func() { _ = boundary.Close() }()
	if limits.Memory != "" {
		if err := writeCgroupFile(boundary, "memory.max", memory); err != nil {
			return nil, err
		}
	}
	if limits.CPU != "" {
		if err := writeCgroupFile(boundary, "cpu.max", cpu); err != nil {
			return nil, err
		}
	}
	info, err := boundary.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("resource boundary identity unavailable")
	}
	result := &sandboxCgroup{Path: dir, Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Memory: memory, CPU: cpu}
	if err := result.verify(); err != nil {
		return nil, err
	}
	keep = true
	return result, nil
}

func openSandboxCgroup(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	var stat unix.Statfs_t
	if err = unix.Fstatfs(fd, &stat); err != nil || stat.Type != unix.CGROUP2_SUPER_MAGIC {
		_ = file.Close()
		return nil, fmt.Errorf("resource boundary must be on the cgroup v2 filesystem")
	}
	return file, nil
}

func (c *sandboxCgroup) open() (*os.File, error) {
	if c == nil {
		return nil, nil
	}
	if !filepath.IsAbs(c.Path) || !strings.HasPrefix(filepath.Base(c.Path), "tclaude-launch-") {
		return nil, fmt.Errorf("invalid resource boundary path")
	}
	file, err := openSandboxCgroup(c.Path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Dev) != c.Device || uint64(stat.Ino) != c.Inode {
		_ = file.Close()
		return nil, fmt.Errorf("resource boundary identity changed")
	}
	return file, nil
}

func (c *sandboxCgroup) verify() error {
	if c == nil {
		return nil
	}
	file, err := c.open()
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	for name, want := range map[string]string{"memory.max": c.Memory, "cpu.max": c.CPU} {
		if want == "" {
			continue
		}
		got, err := readCgroupFile(file, name)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(got)) != want {
			return fmt.Errorf("resource boundary %s changed", name)
		}
	}
	return nil
}

func readCgroupFile(directory *os.File, name string) ([]byte, error) {
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() { _ = file.Close() }()
	return io.ReadAll(io.LimitReader(file, 65536))
}

func writeCgroupFile(directory *os.File, name, value string) error {
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() { _ = file.Close() }()
	_, err = file.WriteString(value)
	return err
}

// remove never recursively traverses a kernel hierarchy or kills a workload.
// It is valid during abort or after the boundary's owner has observed exit.
func (c *sandboxCgroup) remove() error {
	if c == nil {
		return nil
	}
	file, err := c.open()
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return os.Remove(c.Path)
}

func (c *sandboxCgroup) kill() error {
	if c == nil {
		return nil
	}
	file, err := c.open()
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return writeCgroupFile(file, "cgroup.kill", "1")
}

func protectSandboxResourcePaths(inspector *SandboxPathInspector) (*SandboxPathInspector, error) {
	roots := []string{"/sys/fs/cgroup"}
	for _, root := range inspector.roots {
		roots = append(roots, root.configuredPath)
	}
	return NewSandboxPathInspector(roots)
}
