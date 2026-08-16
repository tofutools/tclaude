# History and naming

tclaude began in March 2026 as a set of Claude Code utilities — tmux session
wrapping, conversation search, worktree helpers — carved out of a larger
personal toolbox. It grew from that single-harness utility belt into a
harness-agnostic agentic dev environment: today Claude Code, OpenAI Codex
CLI, OpenCode, and GitHub Copilot CLI all run behind the same workflow, and
the multi-agent machinery (the `agentd` daemon, groups, permissions, audit)
is the center of the project rather than an add-on.

The long version of that evolution, reconstructed from the git log, is told
in [The Making of tclaude](stories/origin-story.md).

## Why so much of it is still called "claude"

The project's origin shows in its names. Many identifiers are Claude-derived
even where the behavior is fully harness-agnostic:

- The binary and module are `tclaude` ("tmux + claude"), and its state lives
  under `~/.tclaude/` — regardless of which harnesses you run.
- Environment variables use the `TCLAUDE_` prefix (`TCLAUDE_SESSION_ID`,
  `TCLAUDE_OLLAMA_URL`, and so on) for every harness.
- Internal package paths sit under `pkg/claude/`, including the
  harness-abstraction seam itself (`pkg/claude/harness`).
- Some persisted defaults echo the origin: for example, conversations
  recorded before harness tracking existed resolve to Claude Code, because
  that was the only harness at the time.

These names are historical, not statements of scope. The project does not
mass-rename them: stable names in config files, environment variables, and
state paths are a compatibility surface, and churning them would break
existing installations for cosmetic gain. Renames happen only at clean,
contained rewrite points.

When reading docs or code, treat "claude" in a path or prefix as the
project's name showing its age. Whether a feature is actually Claude
Code-specific is stated explicitly wherever it matters — the
[capability matrix](harnesses.md) is the authority on what each harness
supports.
