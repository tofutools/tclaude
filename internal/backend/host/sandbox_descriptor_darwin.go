//go:build darwin

package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// sandboxDescriptorInvocation applies the selected filesystem regions through
// Seatbelt. Unlike Linux, macOS has no per-process bind-mount namespace: genuine
// remaps are refused rather than silently interpreting a different guest path.
// Root and provider policy compilation supplies the positive regions first.
func sandboxDescriptorInvocation(wrapper string, child ProcessSpec, bindings *SandboxMountBindings, privateNetwork bool) (ProcessSpec, *os.File, error) {
	if !filepath.IsAbs(wrapper) || !filepath.IsAbs(child.Executable) || !filepath.IsAbs(child.Directory) {
		return ProcessSpec{}, nil, fmt.Errorf("sandbox invocation requires absolute executable and working paths")
	}
	if bindings == nil || len(bindings.files) != len(bindings.pins) || len(child.ExtraFiles) != 0 || !child.ExactEnvironment {
		return ProcessSpec{}, nil, fmt.Errorf("sandbox invocation requires retained sources and an explicit environment")
	}
	// Authored environments already reserve loader/startup controls. Reject them
	// here too: sandbox-exec must not load child-selected code before confinement.
	for _, entry := range child.Env {
		name, _, found := strings.Cut(entry, "=")
		if !found || name == "" || strings.HasPrefix(name, "DYLD_") || strings.HasPrefix(name, "LD_") {
			return ProcessSpec{}, nil, fmt.Errorf("sandbox wrapper environment contains an invalid or loader-control entry")
		}
	}
	args := []string{}
	readRegions := []string{`(literal "/dev/null")`, `(literal "/dev/tty")`, `(literal "/dev/random")`, `(literal "/dev/urandom")`, `(subpath "/dev/fd")`}
	writeRegions := []string{`(literal "/dev/null")`, `(literal "/dev/tty")`}
	selectors := make([]string, len(bindings.pins))
	for index, pin := range bindings.pins {
		guest, err := filepath.EvalSymlinks(pin.Guest)
		if err != nil || guest != pin.Source {
			return ProcessSpec{}, nil, fmt.Errorf("macOS sandbox cannot remap guest path %q", pin.Guest)
		}
		name := "SOURCE_" + strconv.Itoa(index)
		args = append(args, "-D", name+"="+pin.Source)
		predicate := "literal"
		if pin.Kind == "directory" {
			predicate = "subpath"
		}
		selectors[index] = fmt.Sprintf("(%s (param %q))", predicate, name)
		readRegions = append(readRegions, selectors[index])
	}
	// Resolve positive parent/child precedence in the predicates themselves.
	// A read-only descendant carves out its writable ancestor; an explicit
	// writable grandchild has its own region and can be admitted independently.
	for index, pin := range bindings.pins {
		if pin.Access != model.SandboxFilesystemWrite {
			continue
		}
		region := selectors[index]
		for childIndex, descendant := range bindings.pins {
			if descendant.Access == model.SandboxFilesystemRead && sandboxWithin(descendant.Source, pin.Source) {
				region = "(require-all " + region + " (require-not " + selectors[childIndex] + "))"
			}
		}
		writeRegions = append(writeRegions, region)
	}
	// dyld opens the root directory during boot discovery, so metadata-only
	// access is insufficient. This exact vnode is a runtime resource; it does
	// not grant access to any descendant outside the selected readable regions.
	profile := "(version 1)\n(allow default)\n" +
		"(deny file-read* (require-all (require-not (literal \"/\")) (require-not (require-any " + strings.Join(readRegions, " ") + "))))\n" +
		"(deny file-write* (require-not (require-any " + strings.Join(writeRegions, " ") + ")))\n" +
		// Seatbelt mediates Unix connect as network-outbound, not file-read.
		"(deny network-outbound (remote unix-socket (require-not (require-any " + strings.Join(readRegions, " ") + "))))\n"
	if privateNetwork {
		// IP isolation is separate from the filesystem-gated Unix socket axis.
		outbound, inbound := `(remote ip "*:*")`, `(local ip "*:*")`
		if bindings.controlPort != 0 {
			if bindings.controlPort < 1 || bindings.controlPort > 65535 {
				return ProcessSpec{}, nil, fmt.Errorf("invalid sandbox control port")
			}
			// Seatbelt accepts the literal localhost selector, not numeric IP
			// spelling. The native relay still dials only IPv4 loopback.
			endpoint := strconv.Quote("localhost:" + strconv.Itoa(bindings.controlPort))
			outbound = "(require-all " + outbound + " (require-not (remote ip " + endpoint + ")))"
			inbound = "(require-all " + inbound + " (require-not (local ip " + endpoint + ")))"
		}
		profile += "(deny network-outbound " + outbound + ")\n" +
			"(deny network-inbound " + inbound + ")\n" +
			"(deny network-bind " + inbound + ")\n"
	}
	args = append(args, "-p", profile, child.Executable)
	args = append(args, child.Args...)
	wrapped := child
	wrapped.Executable, wrapped.Args = wrapper, args
	wrapped.Env = MergeEnvironment(nil, child.Env)
	return wrapped, nil, nil
}

func sandboxExecInvocation(wrapper string, child ProcessSpec, bindings *SandboxMountBindings, privateNetwork bool) (ProcessSpec, *os.File, error) {
	return sandboxDescriptorInvocation(wrapper, child, bindings, privateNetwork)
}

func sandboxWithin(child, parent string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
