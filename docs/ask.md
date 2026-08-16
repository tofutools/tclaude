# Ask

`tclaude ask` puts a one-shot question to a coding harness without creating
a tmux session or taking over your terminal. It prints the answer, exits
with the harness's status, and keeps a resumable thread for the next
question from the same terminal and directory. All four harnesses — Claude
Code, Codex CLI, OpenCode, and Copilot CLI — have an ask surface.

```bash
# Ask about the current project
tclaude ask "where is authentication enforced?"

# Fold piped input into the question
git diff | tclaude ask "is this safe to merge?"

# Capture a clean answer in a script
summary=$(tclaude ask "summarize this package in one sentence")

# Open the full harness TUI for a back-and-forth
tclaude ask -i "help me refactor this package"
```

Print mode (`-p`, the default) streams or prints the answer and returns to
the shell; it is safe for pipes and command substitution. Interactive mode
(`-i`) attaches the harness's normal TUI to the current terminal — it needs
a real tty. Neither creates a tclaude session; use
[Sessions](sessions.md) when you want detach/reattach.

## Thread continuity

The thread key is the current **terminal plus working directory**, stored in
tclaude's SQLite database. Repeated questions from the same terminal in the
same directory resume one conversation; changing either starts or resumes a
different bucket.

```bash
tclaude ask -w                     # show this bucket: terminal key, cwd,
                                   # current conversation id
tclaude ask -n "new topic: review the db package"  # fresh thread
tclaude ask -n                     # reset the bucket without asking
```

A fresh thread records which harness answered it, and an existing thread
**always keeps its recorded harness**, even if you change the default later
— use `-n` to pick up a new default. If the thread's conversation was
deleted in the harness, the next ask detects the stale mapping and starts
fresh. `-w/--where` on a fresh bucket reports the key without resolving a
harness.

## Harness, model, and effort

A fresh ask defaults to Claude Code with `sonnet` / `medium`. Per-call flags
override model and effort:

```bash
tclaude ask -m haiku -e low "give me the short version"
```

To change the default harness for fresh asks, select a saved spawn profile
in the dashboard (**Config → Ask & scribe defaults**) or set `ask.profile`
in `~/.tclaude/data/config.json`. Only the profile's harness, model, and
effort fields are used; its sandbox, identity, and other spawn fields are
ignored. A deleted profile falls back to the Claude defaults instead of
failing. Without a profile, `ask.model` and `ask.effort` set Claude's
defaults directly.

Resolution order: per-call flags → selected profile → `ask.model` /
`ask.effort` → built-ins.

## Piped input

Piped stdin is appended to the typed question as context. Question and
payload travel as a single process argument behind the harness's
end-of-options guard (or as the prompt-flag value for Copilot), so diff
lines starting with `-` cannot be parsed as CLI flags.

```bash
rg -n "TODO|FIXME" . | tclaude ask "which of these should be fixed first?"
git show HEAD | tclaude ask "write a concise review"
```

## Capture safety

Print mode is designed so a captured answer cannot quietly modify your
workspace:

- **Codex** print runs `codex exec` in a read-only sandbox, with no
  git-repo requirement. It can inspect the directory but not change it.
- **Claude Code** print uses the non-interactive `claude -p` path.
- **Copilot** print runs headless `copilot -p`, buffered — the answer
  arrives whole rather than streaming. tclaude passes no permission flags:
  headless, a tool call that would need approval is simply denied and the
  turn finishes without it. tclaude also unsets `COPILOT_ALLOW_ALL` for
  every ask so that variable cannot promote the turn from your environment.
  This is Copilot's own headless fallback, not an OS sandbox.

Codex and Copilot write verbose transcripts to stderr; tclaude hides them by
default and keeps only the answer on stdout. `-v/--verbose` shows the
transcript, and a failure always reveals it so auth or model errors are not
swallowed.

Codex mints its conversation id on the first turn, so tclaude discovers a
fresh Codex thread's id after the run completes.

Claude's terminal stream uses a paced typewriter renderer; disable that
presentation layer with `--no-smoothing` or `TCLAUDE_ASK_SMOOTH=0` (piped
output is never smoothed).

## Flags

| Flag | Purpose |
|---|---|
| `-p`, `--print` | Print the answer and exit (default) |
| `-i`, `--interactive` | Open the full harness TUI; needs a real terminal |
| `-n`, `--new` | Forget the current terminal/directory thread first |
| `-m`, `--model` | Override the model for this turn |
| `-e`, `--effort` | Override reasoning effort for this turn |
| `-w`, `--where` | Print the current ask bucket and exit |
| `-v`, `--verbose` | Keep the harness's capture transcript on stderr |
| `--no-smoothing` | Disable the paced Claude terminal render |
