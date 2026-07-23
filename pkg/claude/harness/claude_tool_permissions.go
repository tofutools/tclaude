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
	// Break-glass reopens are folded into the read set. The OS layer emits an
	// allowRead/allowWrite for each acknowledged path beneath a deny (see
	// claudeSandboxBlockWithBreakGlass), so on THIS surface they are reopens
	// exactly like ordinary read/write rows — and a deny that is their ancestor
	// is just as unmirrorable. Passing them here is what makes the shape check
	// below see them: an ordinary deny can only ever be an ANCESTOR of a
	// break-glass path (break-glass paths sit inside a protected root, which
	// ordinary deny rows are forbidden to intersect), so a `deny ~` covering an
	// acknowledged `~/.tclaude/data` is caught as a reopen-under-deny and
	// skipped, instead of emitting a Read deny that would defeat the
	// acknowledgement on the built-in tools.
	reopenReads := append(append([]string{}, readDirs...), breakGlassDirs...)
	grants := sandboxpolicy.GrantsFromDirs(
		normalizedSandboxWriteDirs(reopenReads),
		normalizedSandboxWriteDirs(writeDirs),
		denies,
	)
	// Any deny that covers a reopen cannot be mirrored: Claude evaluates deny
	// before allow with no specificity ordering, so the reopen beneath it would
	// be unreachable for the built-in tools.
	unmirrorable := map[string]bool{}
	for _, shape := range sandboxpolicy.ReopensUnderDeny(grants) {
		unmirrorable[shape.Deny] = true
	}
	for _, path := range denies {
		if unmirrorable[path] {
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
//
// The path is glob-escaped first. Read/Edit rules are gitignore PATTERNS, but
// the OS-sandbox side binds the LITERAL path — so a real directory named
// `we[ir]d` would render as `Read(//…/we[ir]d/**)`, whose `[ir]` is a character
// class that matches `weid`/`werd` but not the literal directory, silently
// leaving the built-in tools un-denied for exactly the path Bash is confined
// to. Escaping the metacharacters keeps the two surfaces enforcing the same
// path. (Claude Code does the same escaping when it materializes a rule from an
// approved literal path.)
func claudeAbsolutePermissionPattern(dir string) string {
	trimmed := escapeGitignoreGlobs(strings.TrimPrefix(dir, "/"))
	if trimmed == "" {
		// A root deny. `//path/**` for path="" degenerates to the brittle
		// `///**`; the documented whole-filesystem form is `//**` (`//` anchors
		// at the filesystem root, `**` matches everything beneath). In practice
		// a `deny /` never reaches here — the launch contract always reopens the
		// workspace, so root is a reopen-under-deny and gets skipped — but emit
		// the correct pattern rather than a degenerate one if it ever does.
		return "//**"
	}
	return "//" + trimmed + "/**"
}

// escapeGitignoreGlobs backslash-escapes the gitignore metacharacters so a path
// segment matches literally. `\` is escaped first so it cannot double-escape a
// following metacharacter. `*`, `?`, and `[` are the active pattern operators;
// `]` is escaped too so a literal bracket pair cannot form a class with an
// unescaped partner. The trailing `/**` this feeds into is added by the caller
// and intentionally left as a live recursive glob.
func escapeGitignoreGlobs(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '\\', '*', '?', '[', ']':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
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
