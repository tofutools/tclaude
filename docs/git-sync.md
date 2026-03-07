# Git Sync 🔄

Sync Claude Code conversations across multiple computers using a git repository.

## Overview

This feature keeps `~/.claude/projects_sync` as a git working directory separate from the actual `~/.claude/projects`, giving full control over the merge process.

**Why a separate sync directory?**
- Avoids polluting Claude's data with `.git` files
- Enables intelligent merging of index files
- Lets you review changes before they affect your local conversations

## Quick Start

```bash
# Create a private repo for your conversations
gh repo create my-claude-sync --private

# Initialize sync
tclaude git init git@github.com:username/my-claude-sync.git

# Sync conversations
tclaude git sync
```

## Commands

### git init

Initialize git sync for Claude conversations.

```bash
tclaude git init <repo-url>
```

**What it does:**
- Creates `~/.claude/projects_sync` directory
- If remote is empty: initializes a new git repo
- If remote has content: clones existing conversations

### git sync

Sync local conversations with the remote repository.

```bash
tclaude git sync [flags]
```

**Flags:**

| Flag | Description |
|------|-------------|
| `--keep-local` | On conflict, keep local version without asking |
| `--keep-remote` | On conflict, keep remote version without asking |
| `--dry-run` | Show what would be synced without making changes |

**Sync process:**
1. Pull remote changes into sync directory
2. Merge local conversations into sync (with path canonicalization)
3. Commit changes
4. Push to remote
5. Copy merged results back to local (with path de-canonicalization)

### git status

Show the status of the sync repository.

```bash
tclaude git status
```

Shows:
- Sync directory location
- Remote repository URL
- Uncommitted changes
- Last sync time

### git repair

Merge duplicate project directories caused by different paths across machines.

```bash
tclaude git repair [--dry-run]
```

Uses `~/.claude/sync_config.json` to identify equivalent paths and merge them.

## Cross-Machine Path Mapping

Different machines have different paths (e.g., `/home/gigur` vs `/Users/johkjo`). Configure `~/.claude/sync_config.json` to map equivalent paths:

```json
{
  "homes": ["/home/gigur", "/Users/johkjo"],
  "dirs": [
    ["/home/gigur/git", "/Users/johkjo/git/personal"]
  ]
}
```

**Fields:**
- `homes`: List of equivalent home directories. First entry is canonical.
- `dirs`: Groups of equivalent directories. First entry in each group is canonical.

**How it works:**
- When syncing local → sync: paths are canonicalized
- When syncing sync → local: paths are mapped to local equivalents
- The `repair` command merges any duplicate project dirs

**Example:** On Mac, `/Users/alice/git/myproject` becomes `-home-alice-git-myproject` in the sync repo.

## Merge Strategy

### Session Index Files (`sessions-index.json`)

These files are merged intelligently:
- Union of all conversation entries by `sessionId`
- If the same conversation exists on both sides, keeps the entry with the newer `modified` timestamp

### Conversation Files (`.jsonl`)

- **Remote only**: Copied to local
- **Local only**: Kept
- **Same content**: No action
- **Different content**: Conflict prompt (unless `--keep-local` or `--keep-remote`)

When a conflict occurs, you'll see:
```
Conflict: abc12345.jsonl
  Local:  142 messages
  Remote: 138 messages
Keep which version? [l]ocal / [r]emote / [s]kip:
```

## Typical Workflows

### First-time setup on a new computer

```bash
# Install tclaude
go install github.com/tofutools/tclaude@latest

# Initialize with your existing sync repo
tclaude git init git@github.com:username/my-claude-sync.git

# Pull down your conversations
tclaude git sync
```

### Daily sync

```bash
# Before starting work - pull latest
tclaude git sync

# ... use Claude Code ...

# End of day - push your conversations
tclaude git sync
```

## Privacy Considerations

⚠️ **Your conversations may contain sensitive information!**

- Always use a **private repository**
- Consider what projects/conversations you're syncing
- The sync includes all projects in `~/.claude/projects`

## Troubleshooting

### "git sync not initialized"

Run `tclaude git init <repo-url>` first.

### Merge conflicts on every sync

If the same conversation is being actively used on multiple machines, you'll get conflicts. Options:
- Use `--keep-local` or `--keep-remote` to auto-resolve
- Finish work on one machine before switching

### Large repository

**Note:** Conversations can grow large over time.
