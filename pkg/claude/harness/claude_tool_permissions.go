package harness

import (
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Claude Code enforces filesystem access through TWO independent surfaces, and
// a sandbox profile historically drove only the first:
//
//  1. `sandbox.filesystem.*` — the OS sandbox (bubblewrap/Seatbelt), scoped to
//     Bash commands and their child processes.
//  2. `permissions.deny` `Read(…)` / `Edit(…)` rules — the tool-permission
//     layer, which is what gates the built-in Read/Write/Edit/MultiEdit/
//     NotebookEdit tools. Per Claude Code's permissions docs these "apply to
//     Claude's built-in file tools and to file commands Claude Code recognizes
//     in Bash", but NOT to arbitrary subprocesses.
//
// Rendering only (1) meant an operator-authored `deny ~/.ssh` confined Bash
// while the built-in Read tool could still open ~/.ssh/id_rsa — the profile
// read stricter than it enforced (TCL-666). This file mirrors deny rows onto
// (2) so one authored row binds both surfaces.
//
// The two protected roots (~/.tclaude/data, ~/.claude/sessions) were already
// covered on both layers by the static `tclaude setup --install-sandbox-hardening`
// block; this closes the same gap for everything the operator authors.

// claudeToolPermissionDenyRules renders the `permissions.deny` entries that
// mirror a launch's OS-sandbox deny dirs onto the built-in file tools.
//
// It deliberately does NOT mirror every deny. Claude Code evaluates permission
// rules "in order: deny, then ask, then allow", and "rule specificity doesn't
// change the order" — a deny rule cannot carry allowlist exceptions. That makes
// the reopen-under-deny shape INEXPRESSIBLE on this surface: mirroring a
// `deny ~` would deny the built-in tools every path beneath it, including the
// agent's own workspace, with no allow rule able to reopen it. The OS sandbox
// resolves the same overlap by most-specific-wins, which is why the shape works
// there and not here.
//
// So a deny is mirrored only when no read/write grant sits strictly beneath it
// in the same effective grant set. That covers the leaf-deny postures where the
// gap actually bites — the portable common-rule tier (~/.ssh, ~/.gnupg, cloud
// credentials, VCS tokens, browser profiles) — and skips exactly the shapes it
// would break. skipped names the deny paths that could not be represented, so
// callers can surface them rather than let them look enforced.
func claudeToolPermissionDenyRules(readDirs, writeDirs, denyDirs, breakGlassDirs []string) (rules []string, skipped []string) {
	denies := normalizedSandboxWriteDirs(denyDirs)
	if len(denies) == 0 {
		return nil, nil
	}
	grants := sandboxpolicy.GrantsFromDirs(
		normalizedSandboxWriteDirs(readDirs),
		normalizedSandboxWriteDirs(writeDirs),
		denies,
	)
	// Any deny that appears as the covering deny of a reopen pair cannot be
	// mirrored; the reopen beneath it would be unreachable for the built-in
	// tools.
	unmirrorable := map[string]bool{}
	for _, shape := range sandboxpolicy.ReopensUnderDeny(grants) {
		unmirrorable[shape.Deny] = true
	}
	for _, path := range denies {
		if unmirrorable[path] {
			skipped = append(skipped, path)
			continue
		}
		// A break-glass grant AT the deny path is not a reopen-under-deny —
		// normalization folds a same-path grant into the deny, so the shape
		// check above cannot see it. On the OS surface that pairing still works
		// because allowRead takes precedence over denyRead; on this surface
		// deny always wins, so emitting the rule would silently defeat an
		// acknowledged break-glass on exactly the built-in tools it was
		// acknowledged for.
		if breakGlassCoversPath(breakGlassDirs, path) {
			skipped = append(skipped, path)
			continue
		}
		pattern := claudeAbsolutePermissionPattern(path)
		// Emit BOTH. A Read deny also blocks Edit on the same path in current
		// Claude Code versions, but Write and NotebookEdit are not covered by
		// it, so the explicit Edit rule is what makes "no tool may change this"
		// true across versions. This mirrors the static hardening block.
		rules = append(rules, "Read("+pattern+")", "Edit("+pattern+")")
	}
	sort.Strings(skipped)
	return rules, skipped
}

// claudeAbsolutePermissionPattern converts an absolute directory path into the
// gitignore-style recursive pattern Claude Code's Read/Edit rules expect.
//
// The doubled leading slash is REQUIRED and is the easy thing to get wrong: per
// Claude Code's permissions docs a single leading slash anchors at the settings
// source rather than the filesystem root ("A pattern like /Users/alice/file
// isn't an absolute path"), and for a `--settings` payload that source is the
// session's original cwd. `//path` is the documented absolute form.
func claudeAbsolutePermissionPattern(dir string) string {
	return "//" + strings.TrimPrefix(dir, "/") + "/**"
}

// appendClaudePermissionDeny merges rules into settings["permissions"]["deny"],
// preserving any entries already present and dropping duplicates. The shape
// matches appendSandboxFilesystemDirs so both surfaces merge the same way.
func appendClaudePermissionDeny(settings map[string]any, rules []string) {
	if len(rules) == 0 {
		return
	}
	permissions, _ := settings["permissions"].(map[string]any)
	if permissions == nil {
		permissions = map[string]any{}
		settings["permissions"] = permissions
	}
	existing, _ := permissions["deny"].([]any)
	seen := make(map[string]bool, len(existing)+len(rules))
	out := make([]any, 0, len(existing)+len(rules))
	for _, value := range existing {
		rule, ok := value.(string)
		if !ok || seen[rule] {
			continue
		}
		seen[rule] = true
		out = append(out, rule)
	}
	for _, rule := range rules {
		if !seen[rule] {
			seen[rule] = true
			out = append(out, rule)
		}
	}
	permissions["deny"] = out
}
