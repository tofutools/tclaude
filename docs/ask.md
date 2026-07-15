# Ask from the shell

`tclaude ask` puts a question to a coding harness without creating a tmux
session or taking over your terminal. It prints the answer, exits with the
harness's status, and keeps a resumable thread for the next question from the
same terminal and working directory.

Both Claude Code and OpenAI Codex CLI are supported.

## Everyday use

```bash
# Ask about the current project
tclaude ask "where is authentication enforced?"

# Fold piped input into the question
git diff | tclaude ask "is this safe to merge?"

# Capture a clean answer in a script
summary=$(tclaude ask --print "summarize this package in one sentence")

# Open the full harness TUI for a back-and-forth
tclaude ask --interactive "help me refactor this package"
```

Print mode is the default. On a terminal, Claude Code's answer streams as it is
generated; Codex prints its final answer when `codex exec` completes. Redirected
or captured output is buffered and contains only the answer.

## Thread continuity

The thread key is the current **terminal plus working directory**. Repeated
questions from the same terminal in the same directory resume one conversation;
changing either starts or resumes a different bucket.

```bash
# Show the resolved terminal key and cwd, plus any existing thread's harness/id
tclaude ask --where

# Forget this bucket and start a fresh conversation
tclaude ask --new "new topic: review the database package"

# Reset the bucket without submitting a question
tclaude ask --new
```

`--where` reports a harness and conversation ID only when the bucket already
has a mapped thread. For a fresh bucket it does not load or resolve the
configured default harness.

The mapping lives in tclaude's SQLite database. If its conversation was deleted
from the harness, the next ask detects the stale mapping and starts fresh.

## Print and interactive modes

| Mode | Command | Behavior |
|---|---|---|
| Print | `tclaude ask ...` or `--print` | Answer and return to the shell; safe for pipes and command substitution |
| Interactive | `tclaude ask --interactive ...` | Attach the harness TUI directly to the current terminal for questions, approvals, or edits |

Codex print mode runs `codex exec` with a **read-only sandbox** and no Git-repo
requirement. It can inspect the current directory to answer a question but
cannot modify it. Interactive mode inherits your normal Codex configuration
because a human is present to handle approvals.

Claude Code print mode uses its non-interactive `-p` path. Interactive mode
opens the normal Claude Code TUI. Neither mode creates a tclaude tmux session;
use [Session management](sessions.md) when you want detach/reattach behavior.

## Choose a harness, model, and effort

A fresh ask uses Claude Code with the built-in `sonnet` / `medium` defaults
unless configured otherwise. Per-call flags override model and effort:

```bash
tclaude ask --model haiku --effort low "give me the short version"
tclaude ask --model opus --effort high "analyze this concurrency design"
```

To make fresh asks use Codex, select a saved **spawn profile** in the dashboard's
**Config → Ask defaults** section. Only the profile's harness, model, and effort
are used; its agent name, role, sandbox, and other spawn fields are ignored.
The setting is stored in `~/.tclaude/data/config.json` as the profile name:

```json
{
  "ask": {
    "profile": "codex-fast"
  }
}
```

The profile itself lives in tclaude's profile library, editable from the
dashboard or `tclaude agent profiles`. If a selected profile is deleted or
renamed, ask falls back to the default Claude configuration instead of failing
every invocation.

An existing ask thread always keeps its recorded harness. Changing the default
only affects a fresh bucket; use `--new` to start one after changing profiles.

Without a profile, you can set Claude's defaults directly:

```json
{
  "ask": {
    "model": "sonnet",
    "effort": "medium"
  }
}
```

Resolution is: per-call model/effort flags → selected profile → `ask.model` /
`ask.effort` → built-in defaults. For a Codex profile with blank model or effort,
tclaude omits the corresponding flags and lets Codex use its own configuration.

## Piped input and output

Piped stdin is appended to the typed question as context. The question and
payload are passed as one process argument behind the harness's end-of-options
guard, so diff lines or content beginning with `-` are not parsed as CLI flags.

```bash
rg -n "TODO|FIXME" . | tclaude ask "which of these should be fixed first?"
git show HEAD | tclaude ask "write a concise review"
```

Codex writes a verbose execution transcript to stderr in print mode. tclaude
hides it by default while preserving the clean answer on stdout. Use `--verbose`
to see the transcript; failures always reveal it so authentication or model
errors are not swallowed.

Claude's terminal stream uses a paced, character-by-character renderer by
default. Disable that presentation layer without changing captured output:

```bash
tclaude ask --no-smoothing "explain the parser"
TCLAUDE_ASK_SMOOTH=0 tclaude ask "explain the parser"
```

## Flag reference

| Flag | Purpose |
|---|---|
| `-p`, `--print` | Print the answer and exit (the default) |
| `-i`, `--interactive` | Open the full harness TUI; requires a real terminal |
| `-n`, `--new` | Forget the current terminal/directory thread before asking |
| `-m`, `--model` | Override the configured model for this turn |
| `-e`, `--effort` | Override reasoning effort for this turn |
| `-w`, `--where` | Print the current ask bucket and exit |
| `-v`, `--verbose` | Keep the harness's capture transcript on stderr |
| `--no-smoothing` | Disable paced Claude terminal output |

Run `tclaude ask --help` for the live reference supported by the installed
version.
