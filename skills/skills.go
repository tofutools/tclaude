// Package skills holds optional utility skills shipped with tclaude. Unlike the
// coordination skills bundled in pkg/claude/agent, they are not installed by
// default: `tclaude setup --install-utility-skills` writes them into the
// Claude Code and Codex CLI user skill directories on request.
//
// Each skill is a directory holding a SKILL.md (plus any helper files). To add
// one, create the directory, list it in Names, and add it to the embed below.
package skills

import "embed"

//go:embed demo-recording
var FS embed.FS

// Names lists the utility skills, in install order.
var Names = []string{
	"demo-recording",
}
