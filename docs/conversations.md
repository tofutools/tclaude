# Conversation Management 💬

Browse and search Claude Code and OpenAI Codex CLI conversations together.
Each indexed conversation records its harness, so resume and watch-mode launch
use the correct CLI automatically.

Listing, text search, native archive visibility, and resume are
harness-agnostic. The `archive` / `unarchive` commands, semantic index, and
physical `delete`, `prune-empty`, `cp`, and `mv` commands still operate on
Claude Code's indexed project `.jsonl` store only; those sections are labelled
explicitly below.

## Commands

### conv ls

List conversations.

```bash
# List conversations for current project
tclaude conv ls

# Interactive watch mode
tclaude conv ls -w

# Global - all projects
tclaude conv ls -g

# Global interactive
tclaude conv ls -g -w

# Limit results
tclaude conv ls -n 10

# Filter by time
tclaude conv ls --since 7d
tclaude conv ls --before 2024-01-01
```

**Flags:**

| Flag | Description |
|------|-------------|
| `-w, --watch` | Interactive watch mode |
| `-g, --global` | Search all projects |
| `-n, --limit` | Limit number of results |
| `--since` | Show only after this time |
| `--before` | Show only before this time |
| `--show-archived` | Include Claude conversations hidden by tclaude archival and Codex conversations archived in Codex's native store |

### conv watch

Shortcut for `conv ls -w` — jumps straight into interactive watch mode. Takes the same
`-g`, `--since`, and `--before` flags as `conv ls`.

```bash
tclaude conv watch
tclaude conv watch -g
```

### conv search

Search conversation content.

```bash
# Search in current project
tclaude conv search "authentication"

# Search globally
tclaude conv search -g "authentication"

# Search with time filter
tclaude conv search --since 24h "bug fix"

# Full content search (slower, more thorough)
tclaude conv search --content "specific error message"
```

`--content` reads uncompressed transcript/rollout files in addition to the
metadata search. Cold Codex rollouts stored as compressed `.jsonl.zst` files are
not decompressed, so those conversations are searched by title, summary, and
first prompt only.

**Flags:**

| Flag | Description |
|------|-------------|
| `-g, --global` | Search all projects |
| `--content` | Also search uncompressed conversation content |
| `--since` | Filter by time |
| `--before` | Filter by time |

### conv resume

Resume a conversation in a new tmux session through its recorded harness.

```bash
# Resume by ID (creates a new tmux session)
tclaude conv resume <conv-id>

# Resume detached
tclaude conv resume -d <conv-id>
```

Unlike the lower-level `session new --resume`, this command searches all
supported harness stores and selects the recorded harness automatically. The
direct equivalents are:

```bash
tclaude session new --resume <claude-conv-id>
tclaude session new --harness codex --resume <codex-conv-id>
```

The durable conversation record is independent of tmux process history.
Removing an exited row from `session ls -w` therefore does not remove the
conversation's harness, resume provenance, or unmanaged launch fallback.
Standalone conversation listing, search, archive, and resume do not require an
agent record.

### conv archive / unarchive (Claude Code only)

Hide a conversation from default listings without deleting its harness data:

```bash
tclaude conv archive <conv-id>
tclaude conv ls -g --show-archived
tclaude conv unarchive <conv-id>
```

Archiving stamps tclaude's SQLite conversation index only; the Claude Code
`.jsonl` stays in place. It is reversible, and reincarnation hides a Claude
predecessor through the same mechanism. Codex archive state comes from Codex's
own thread store and is visible through `--show-archived`; these commands do not
change that native state.

### conv delete (Claude Code only)

Delete a conversation.

```bash
# Delete by ID
tclaude conv delete <conv-id>

# Skip confirmation
tclaude conv delete -y <conv-id>

# Search globally
tclaude conv delete -g <conv-id>
```

### conv prune-empty (Claude Code only)

Delete conversations with no messages.

```bash
# Prune current project
tclaude conv prune-empty

# Prune globally
tclaude conv prune-empty -g

# Preview only (dry run)
tclaude conv prune-empty --dry-run
```

### conv cp / conv mv (Claude Code only)

Copy or move conversations.

```bash
# Copy a conversation
tclaude conv cp <conv-id> /path/to/dest

# Move a conversation
tclaude conv mv <conv-id> /path/to/dest
```

These commands transform Claude Code's cwd-indexed transcript files. They do
not copy or move Codex rollout/state-store conversations.

## Interactive Watch Mode

Press `w` or use `-w` flag to enter interactive mode.

### Navigation

| Key | Action |
|-----|--------|
| `↑`/`k` | Move up |
| `↓`/`j` | Move down |
| `PgUp`/`Ctrl+B` | Page up |
| `PgDn`/`Ctrl+F` | Page down |
| `g`/`Home` | Go to first |
| `G`/`End` | Go to last |
| `Enter` | Create/attach to session |
| `q`/`Esc` | Quit |

### Search

| Key | Action |
|-----|--------|
| `/` | Start text search (title, prompt, project, branch, ID) |
| `s` | Start [semantic search](semantic-search.md) (requires Ollama) |
| `Esc` | Clear search / exit search or semantic mode |
| `Ctrl+U` | Clear search input |
| `↑`/`↓` | Exit search and navigate |

Text search (`/`) matches against: title, first prompt, project path, git branch, session ID.

Semantic search (`s`) finds conversations by meaning using local embeddings. See [Semantic Search](semantic-search.md) for setup and details.
It currently indexes Claude Code transcripts only.

### Actions

| Key | Action |
|-----|--------|
| `Del`/`x` | Delete conversation |
| `r` | Refresh list |
| `h`/`?` | Show help |

### Delete Confirmation

When deleting a conversation:

- **No active session:** `y` to confirm, `n` to cancel
- **Has active session:**
  - `y` - Delete conversation AND stop session
  - `s` - Stop session only (keep conversation)
  - `n` - Cancel

## Session Indicators ⚡

In the conversation list, indicators show session status:

| Indicator | Meaning |
|-----------|---------|
| ⚡ | Conversation has an attached session (someone's watching!) |
| ○ | Conversation has an active session (running in background) |
| (none) | No active session |

## Time Filters

Commands support flexible time formats:

| Format | Example | Description |
|--------|---------|-------------|
| Duration | `24h`, `7d`, `2w` | Relative time |
| Date | `2024-01-15` | Specific date |
| DateTime | `2024-01-15T10:30` | Date and time |

```bash
# Last 24 hours
tclaude conv ls --since 24h

# Last week
tclaude conv ls --since 7d

# Before a specific date
tclaude conv ls --before 2024-01-01

# Date range
tclaude conv ls --since 2024-01-01 --before 2024-02-01
```

## Session ID Formats

Conversations can be referenced by:

- **Full ID:** `abc12345-def6-7890-abcd-ef1234567890`
- **Short prefix:** `abc12345` (if unique)
- **Autocomplete format:** `abc12345_[project]_prompt...` (from shell completion)

Shell completions automatically provide the full format with context.
