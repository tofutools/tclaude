//go:build linux || darwin

package host

import "fmt"

// Inherited visibility is a host-owned boundary property, never an authored
// mount exception for protected state. Old artifacts default to sparse roots.
func (i *SandboxPathInspector) setSandboxRoot(bound *SandboxMountBindings, inherited, privateNetwork bool) error {
	if inherited && privateNetwork {
		return fmt.Errorf("isolated network requires a constructed sandbox root")
	}
	bound.inheritedRoot = inherited
	bound.protectedRoots = nil
	if inherited {
		for _, root := range i.roots {
			bound.protectedRoots = append(bound.protectedRoots, root.path)
		}
	}
	return nil
}

func sandboxBoundaryOverlays(bound *SandboxMountBindings) []sandboxOverlay {
	overlays := append([]sandboxOverlay(nil), bound.overlays...)
	for _, root := range bound.protectedRoots {
		overlays = append(overlays, sandboxOverlay{Kind: "deny", Path: root})
	}
	return overlays
}
