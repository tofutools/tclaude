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
	for _, overlay := range bindings.overlays {
		if overlay.Kind != "deny" {
			return ProcessSpec{}, nil, fmt.Errorf("macOS Seatbelt cannot create temporary filesystem mounts")
		}
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
	var readRegions []string
	for _, path := range []string{"/dev/null", "/dev/tty", "/dev/random", "/dev/urandom", "/dev/fd"} {
		kind := "literal"
		if path == "/dev/fd" {
			kind = "subpath"
		}
		readRegions = append(readRegions, sandboxDarwinRuntimeRegion(kind, path, bindings.overlays))
	}
	if bindings.controlPort != 0 {
		// lsof inspects the device directory before reporting TCP ownership.
		// Permit only that directory vnode, not its device descendants.
		readRegions = append(readRegions, sandboxDarwinRuntimeRegion("literal", "/dev", bindings.overlays))
	}
	writeRegions := []string{sandboxDarwinRuntimeRegion("literal", "/dev/null", bindings.overlays), sandboxDarwinRuntimeRegion("literal", "/dev/tty", bindings.overlays)}
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
		region := sandboxDarwinDenyCarveouts(selectors[index], pin.Source, bindings.overlays, index >= len(bindings.pins)-bindings.providerCount)
		readRegions = append(readRegions, region)
	}
	// Resolve positive parent/child precedence in the predicates themselves.
	// A read-only descendant carves out its writable ancestor; an explicit
	// writable grandchild has its own region and can be admitted independently.
	for index, pin := range bindings.pins {
		if pin.Access != model.SandboxFilesystemWrite {
			continue
		}
		region := sandboxDarwinDenyCarveouts(selectors[index], pin.Source, bindings.overlays, index >= len(bindings.pins)-bindings.providerCount)
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
		"(deny file-write* (require-not (require-any " + strings.Join(writeRegions, " ") + ")))\n"
	if bindings.controlPort == 0 {
		// Seatbelt mediates Unix connect as network-outbound, not file-read.
		profile += "(deny network-outbound (remote unix-socket (require-not (require-any " + strings.Join(readRegions, " ") + "))))\n"
	} else {
		if bindings.controlPort < 1 || bindings.controlPort > 65535 {
			return ProcessSpec{}, nil, fmt.Errorf("invalid sandbox control port")
		}
		// Keep both destination exceptions inside ONE deny predicate, as in
		// the retained Darwin native-server floor. A separate Unix-path deny
		// can also reject TCP I/O despite a second rule's TCP exception.
		ipException := `(remote ip "*:*")`
		if privateNetwork {
			ipException = "(remote tcp " + strconv.Quote("localhost:"+strconv.Itoa(bindings.controlPort)) + ")"
		}
		profile += "(deny network-outbound (require-all (require-not (remote unix-socket (require-any " + strings.Join(readRegions, " ") + "))) (require-not " + ipException + ")))\n"
	}
	if privateNetwork {
		// IP isolation is separate from the filesystem-gated Unix socket axis.
		if bindings.controlPort == 0 {
			profile += "(deny network-outbound (remote ip \"*:*\"))\n" +
				"(deny network-inbound (local ip \"*:*\"))\n" +
				"(deny network-bind (local ip \"*:*\"))\n"
		} else {
			if bindings.controlPort < 1 || bindings.controlPort > 65535 {
				return ProcessSpec{}, nil, fmt.Errorf("invalid sandbox control port")
			}
			// Match the retained native server contract. Seatbelt's TCP
			// localhost selector is host-wide, not strictly 127.0.0.1; the
			// native relay itself still uses and proves IPv4 loopback.
			endpoint := strconv.Quote("localhost:" + strconv.Itoa(bindings.controlPort))
			profile += "(deny network-bind (require-not (local tcp " + endpoint + ")))\n"
			// Inbound filtering is not a reliable listener/reply boundary on
			// Darwin. The retained contract prevents other listeners at bind.
		}
	}
	args = append(args, "-p", profile, child.Executable)
	args = append(args, child.Args...)
	wrapped := child
	wrapped.Executable, wrapped.Args = wrapper, args
	wrapped.Env = MergeEnvironment(nil, child.Env)
	return wrapped, nil, nil
}

// Each positive region excludes narrower denies. Explicit positive children
// are separate union members, so they reopen exactly their own subtree without
// relying on Seatbelt's rule-order precedence.
func sandboxDarwinDenyCarveouts(region, path string, overlays []sandboxOverlay, provider bool) string {
	for _, overlay := range overlays {
		if sandboxWithin(overlay.Path, path) && !(provider && overlay.Path == path) {
			region = fmt.Sprintf("(require-all %s (require-not (subpath %s)))", region, strconv.Quote(overlay.Path))
		}
	}
	return region
}

func sandboxDarwinRuntimeRegion(kind, path string, overlays []sandboxOverlay) string {
	region := fmt.Sprintf("(%s %s)", kind, strconv.Quote(path))
	for _, overlay := range overlays {
		if sandboxWithin(overlay.Path, path) || sandboxWithin(path, overlay.Path) {
			region = fmt.Sprintf("(require-all %s (require-not (subpath %s)))", region, strconv.Quote(overlay.Path))
		}
	}
	return region
}

func sandboxExecInvocation(wrapper string, child ProcessSpec, bindings *SandboxMountBindings, privateNetwork bool) (ProcessSpec, *os.File, error) {
	return sandboxDescriptorInvocation(wrapper, child, bindings, privateNetwork)
}

func sandboxWithin(child, parent string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
