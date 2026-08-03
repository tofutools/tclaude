package harness

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Translating the Copilot sandbox baseline (TCL-975) into tclaude's outer
// bubblewrap/Seatbelt boundary (TCL-978).
//
// The catalog answers "which paths, in which mode, resolved how". This file
// answers the one question a mount plan additionally needs: what each row
// becomes as a filesystem grant. It is deliberately a TRANSLATION and not a
// second resolver — every path, every access mode and every refusal comes from
// CopilotSandboxBaseline, so the outer layer and Copilot's own policy (TCL-977)
// can never disagree about what a confined Copilot launch may reach.
//
// Three properties are load-bearing enough to state:
//
//   - The EXECUTE bit survives. sandboxpolicy has no execute axis because
//     neither backend takes one: bubblewrap binds are exec-capable unless
//     mounted noexec (tclaude never does), and Seatbelt's process-exec* is
//     allowed for readable paths. So an rwx row maps to an ordinary write
//     grant — but only after this file has CHECKED that it does, because the
//     row that needs it (the package cache, which holds ripgrep, tgrep and
//     prebuilt native modules) would fail in a confusing, tool-search-shaped
//     way rather than a permission-shaped one if a future noexec mount landed.
//   - Row IDs are a KIND, not a key. A launch with several agentd endpoints
//     produces several rows carrying CopilotBaselineAgentdSocket, so this file
//     iterates the list; keying a map by ID would silently drop every endpoint
//     but the last.
//   - The refusal set is part of the contract. This function never repairs,
//     narrows or drops a row the catalog refused: a *SandboxCapabilityError
//     from the baseline is returned unchanged, so a grant set is either
//     complete or absent.

// CopilotTclaudeLayerGrantSet is the outer-layer view of one Copilot launch.
type CopilotTclaudeLayerGrantSet struct {
	// Grants are the filesystem rules the launch contract composes into its
	// mount plan, in catalog order.
	Grants []sandboxpolicy.FilesystemGrant
	// Entries is the catalog the grants were translated from, so a launch can
	// disclose WHY a path is in the plan (Source/Purpose/Evidence) without
	// re-resolving anything.
	Entries []CopilotBaselineEntry
	// ExecutablePaths are the rows whose access includes execute. They are
	// surfaced separately because "this bind must remain exec-capable" is an
	// invariant a mount-plan test asserts, and it is not recoverable from a
	// FilesystemGrant once the execute bit has been folded into read/write.
	ExecutablePaths []string
}

// CopilotTclaudeLayerGrants resolves the Copilot baseline for one launch and
// returns it as outer-layer filesystem grants.
//
// It adds no path of its own. In particular it does NOT add the workspace: the
// catalog refuses to cover it precisely so the workspace grant stays the
// caller's (a launch cwd, a repo, a worktree), which the launch contract
// already composes from its own inputs.
func CopilotTclaudeLayerGrants(
	in CopilotBaselineInput,
) (CopilotTclaudeLayerGrantSet, error) {
	entries, err := CopilotSandboxBaseline(in)
	if err != nil {
		// Returned unchanged: it is already a *SandboxCapabilityError with a
		// stable kind, and wrapping it would cost the daemon the wire code it
		// renders the remedy from.
		return CopilotTclaudeLayerGrantSet{}, err
	}
	out := CopilotTclaudeLayerGrantSet{
		Grants:  make([]sandboxpolicy.FilesystemGrant, 0, len(entries)),
		Entries: entries,
	}
	for _, entry := range entries {
		grant, err := copilotBaselineGrant(entry)
		if err != nil {
			return CopilotTclaudeLayerGrantSet{}, err
		}
		out.Grants = append(out.Grants, grant)
		if entry.Access.Execute {
			out.ExecutablePaths = append(out.ExecutablePaths, entry.Path)
		}
	}
	return out, nil
}

// copilotBaselineGrant maps one catalog row onto one filesystem rule.
//
// The access mapping is not a lookup table by row id, because the row ids are
// deliberately semantic ("what is this path") while the grant is mechanical
// ("what may the process do to it"). Reading the mode off Access keeps a row
// that later changes mode from needing an edit here as well — and keeps a row
// that names an IMPOSSIBLE mode (write-only, execute-only) a loud refusal
// rather than a silently downgraded grant.
func copilotBaselineGrant(
	entry CopilotBaselineEntry,
) (sandboxpolicy.FilesystemGrant, error) {
	if !entry.Access.Read {
		// Every mode the catalog can legitimately produce includes read: a
		// bind the process may write but not read is not something bubblewrap
		// or Seatbelt expresses, and an execute-without-read row would need the
		// loader to read the file it is executing anyway.
		return sandboxpolicy.FilesystemGrant{}, copilotGrantError(fmt.Sprintf(
			"Copilot baseline entry %q requires access %s, which the outer layer cannot express: "+
				"every grant it emits is readable", entry.ID, entry.Access))
	}
	path := filepath.Clean(strings.TrimSpace(entry.Path))
	if path == "" || path == "." || !filepath.IsAbs(path) {
		// Unreachable through CopilotSandboxBaseline, which validates this
		// itself. Kept because this function is the boundary a future caller
		// could reach with a hand-built entry, and a relative path would become
		// a mount rule resolved against whatever cwd the daemon happens to hold.
		return sandboxpolicy.FilesystemGrant{}, copilotGrantError(fmt.Sprintf(
			"Copilot baseline entry %q does not name an absolute path (got %q)", entry.ID, entry.Path))
	}
	access := sandboxpolicy.AccessRead
	if entry.Access.Write {
		access = sandboxpolicy.AccessWrite
	}
	// MountPath is deliberately left empty: every Copilot row is a same-path
	// grant. Copilot resolves these paths itself, from COPILOT_HOME and the XDG
	// variables, so projecting one onto a different guest path would hand the
	// CLI a directory at an address it will never look at.
	return sandboxpolicy.FilesystemGrant{Path: path, Access: access}, nil
}

// SandboxCapabilityCopilotGrantTranslation is the stable wire vocabulary for a
// baseline row the outer layer cannot express. It is distinct from the
// baseline's own kinds because the remedy is different: those name a
// misconfigured environment, this names a tclaude-side gap.
const SandboxCapabilityCopilotGrantTranslation = "copilot-sandbox-grant-translation"

func copilotGrantError(message string) *SandboxCapabilityError {
	return &SandboxCapabilityError{
		Harness: CopilotName,
		Kind:    SandboxCapabilityCopilotGrantTranslation,
		Message: message,
	}
}
