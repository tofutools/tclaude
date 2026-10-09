// Package startupctx renders an agent's startup context: the identity line
// and briefing body agentd hands a freshly spawned agent, and the re-injection
// message tclaude queues when a compaction or /clear boundary has summarized
// or dropped that first turn.
//
// Both renderings live here so they cannot drift apart. The package depends
// only on the database layer, which lets the hook-callback process (package
// session, which cannot import agentd) compose a re-injection itself.
package startupctx

import (
	"fmt"
	"strings"
)

// IdentityPrefix builds the identity/orientation sentence shared by the spawn
// welcome and the post-compaction re-injection: attribution, name, role,
// group, description, sub-repo worktree note, and the `tclaude agent`
// pointer. It has no [system: ...] wrapper and no trailing newline; callers
// append their own closing instruction.
//
// attribution opens the sentence ("spawned by the human", "spawned by po").
func IdentityPrefix(attribution, name, role, descr, groupName, worktreePath, worktreeBranch string) string {
	parts := []string{attribution}
	if name != "" {
		parts = append(parts, fmt.Sprintf("as %q", name))
	}
	if role != "" {
		parts = append(parts, fmt.Sprintf("(role: %s)", role))
	}
	if groupName != "" {
		parts = append(parts, fmt.Sprintf("in group %q", groupName))
	}
	body := strings.Join(parts, " ") + "."
	if descr != "" {
		body += " Descr: " + descr + "."
	}
	// When the spawn targeted a sub-repo of a monorepo launch dir, the
	// agent's cwd is the parent dir but its code work belongs in the
	// worktree. Spell that out so it doesn't edit the parent's repos.
	if worktreePath != "" {
		body += " Your git worktree for code changes is at " + worktreePath
		if worktreeBranch != "" {
			body += " (branch " + worktreeBranch + ")"
		}
		body += " — make code edits there, not elsewhere under your start directory."
	}
	body += " Use `tclaude agent` commands (whoami / --help / inbox ls) to introspect and coordinate."
	return body
}

// ContextBody is the startup briefing body for a freshly-spawned agent's
// inbox. It stitches together up to four sections — the group's shared
// context, profile-specific guidance, the per-spawn task brief and the
// attached files — under plain-text headers, with dividers between them.
//
// Every input may be empty (or whitespace-only); when all are empty, the
// result is "" and the caller skips the inbox insert entirely, so an
// agent with nothing to brief never gets an empty message.
func ContextBody(groupName, groupContext, profileContext, initialMessage string, attachments []string) string {
	groupContext = strings.TrimSpace(groupContext)
	profileContext = strings.TrimSpace(profileContext)
	initialMessage = strings.TrimSpace(initialMessage)

	var sections []string
	if groupContext != "" {
		sections = append(sections, fmt.Sprintf(
			"Group %q startup context — shared guidance for every agent spawned into this group:\n\n%s",
			groupName, groupContext))
	}
	if profileContext != "" {
		sections = append(sections,
			"Agent preset startup context — guidance attached to this agent's selected profile and role:\n\n"+profileContext)
	}
	if initialMessage != "" {
		sections = append(sections, "Your task brief:\n\n"+initialMessage)
	}
	if s := AttachmentsSection(attachments); s != "" {
		sections = append(sections, s)
	}
	return strings.Join(sections, sectionDivider)
}

const sectionDivider = "\n\n---\n\n"

// AttachmentsSection renders the briefing's "Attached files" block from a
// list of file paths, or "" when there are none. The paths were written to a
// temp dir by the dashboard's upload endpoint and are listed so the new agent
// can open them with its own Read tool on the first turn — the daemon never
// reads them itself. Rendered as a markdown bullet list so it stays readable
// both inline in the launch prompt and in `tclaude agent inbox read`.
func AttachmentsSection(attachments []string) string {
	var lines []string
	for _, a := range attachments {
		if a = strings.TrimSpace(a); a != "" {
			lines = append(lines, "- "+a)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "Attached files:\n\n" + strings.Join(lines, "\n")
}
