package session

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// renderMountPlanInterim is the one temporary profile→IR seam used by the
// walking skeleton.
//
// TODO(TCL-751): replace this single call site with
// sandboxpolicy.RenderMountPlan and delete this function once the authoritative
// renderer lands.
func renderMountPlanInterim(e sandboxpolicy.EffectiveProfile) (sandboxpolicy.MountPlan, error) {
	entries := make([]sandboxpolicy.MountEntry, 0,
		len(e.Filesystem)+len(e.BreakGlassFilesystem))
	for _, grant := range e.Filesystem {
		entries = append(entries, sandboxpolicy.MountEntry{
			Path: grant.Path,
			Mode: mountModeForAccess(grant.Access),
		})
	}
	// Break-glass grants are explicit acknowledged reopens of protected paths.
	// Append them before the stable ancestor sort so an exact-path ordinary deny
	// remains earlier and the acknowledged reopen wins.
	for _, grant := range e.BreakGlassFilesystem {
		entries = append(entries, sandboxpolicy.MountEntry{
			Path: grant.Path,
			Mode: mountModeForAccess(grant.Access),
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		leftDepth := mountPathDepth(entries[i].Path)
		rightDepth := mountPathDepth(entries[j].Path)
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return entries[i].Path < entries[j].Path
	})
	return sandboxpolicy.MountPlan{Entries: entries}, nil
}

func mountModeForAccess(access sandboxpolicy.Access) sandboxpolicy.MountMode {
	switch access {
	case sandboxpolicy.AccessWrite:
		return sandboxpolicy.MountRW
	case sandboxpolicy.AccessDeny:
		return sandboxpolicy.MountHide
	default:
		return sandboxpolicy.MountRO
	}
}

func mountPathDepth(path string) int {
	path = filepath.Clean(path)
	if path == string(filepath.Separator) {
		return 0
	}
	return strings.Count(strings.Trim(path, string(filepath.Separator)), string(filepath.Separator)) + 1
}
