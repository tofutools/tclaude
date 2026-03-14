# Semantic Search

Search conversations by meaning, not just keywords. Powered by local embeddings via [Ollama](https://ollama.com) — your data never leaves your machine.

## Why Semantic Search?

When searching past conversations, you rarely remember exact keywords — you remember the *subject* or *concept* you discussed. Keyword search (`conv search`) requires exact text matches, which often misses what you're looking for.

Semantic search embeds your conversations into vector space and finds matches by meaning. Searching for "auth token refresh" will find conversations about authentication and OAuth even if those exact words weren't used.

**Key properties:**

- **Private** — all processing happens locally, conversation data never leaves your machine
- **Fast** — sub-second search once indexed
- **Free** — zero per-query cost after initial setup
- **Offline** — works without internet after the model is downloaded

## Prerequisites

Semantic search requires [Ollama](https://ollama.com) running locally with an embedding model.

### 1. Install Ollama

=== "macOS (Homebrew)"

    ```bash
    brew install ollama
    ```

=== "Linux / WSL"

    ```bash
    curl -fsSL https://ollama.com/install.sh | sh
    ```

### 2. Start Ollama

```bash
# Start as a background service (recommended — auto-starts on login)
brew services start ollama

# Or run manually in foreground
ollama serve
```

### 3. Pull the embedding model

```bash
ollama pull nomic-embed-text
```

This downloads the `nomic-embed-text` model (~274MB, one-time). It has an 8K token context window, runs in ~0.5GB RAM (unloads after 5 minutes of idle), and produces embeddings in under 100ms on Apple Silicon.

### 4. Verify

```bash
curl -s http://localhost:11434/api/embed \
  -d '{"model": "nomic-embed-text", "input": "hello world"}' \
  | python3 -c "import sys,json; e=json.load(sys.stdin)['embeddings'][0]; print(f'{len(e)} dimensions')"
# Expected: 768 dimensions
```

## Getting Started

### Build the index

Before searching, you need to index your conversations:

```bash
# Index conversations for the current project
tclaude conv index-embeddings

# Index all projects
tclaude conv index-embeddings -g
```

Indexing chunks each conversation into pieces (metadata + content at turn boundaries), embeds them via Ollama, and stores the vectors in SQLite. It's incremental — only new or modified conversations are re-indexed on subsequent runs.

```
Indexing 48 conversations (15 already indexed)...
  [1/48] a9a330a8 please unzip all the zip files - 1 chunks (66ms)
  [2/48] 1211af48 hello - 2 chunks (44ms)
  ...
Done in 5.6s: 48 conversations, 78 chunks, 0 errors
```

### Search

```bash
# Search by meaning
tclaude conv search-embeddings "auth token refresh"

# Search all projects
tclaude conv search-embeddings -g "kubernetes deployment"

# Show matching chunk text
tclaude conv search-embeddings -l "database migration"

# JSON output
tclaude conv search-embeddings --json "API rate limiting"
```

Aliases: `sem`, `semantic`, `ai`, `ask`, `ai-search`

## Interactive Watch Mode

Semantic search is integrated into the interactive conversation browser (`tclaude conv ls -w`).

### Usage

1. Press **`s`** to start a semantic search
2. If Ollama is running, tclaude checks how many conversations need indexing
3. If unindexed conversations exist, you'll be prompted: `"N of M conversations not indexed. Index now? [y/n]"`
    - **`y`** — indexes with a progress bar, then shows the search input
    - **`n`** — skips indexing and searches what's already indexed
4. Type your query in the cyan `Semantic: [query_]` input and press **Enter**
5. Results are displayed sorted by similarity, with a **SCORE** column added to the table
6. You can select and attach to any result with **Enter**, just like normal mode

### Exiting Semantic Mode

| Key | Action |
|-----|--------|
| `Esc` | Exit semantic results, return to normal listing |
| `/` | Switch to text search |
| `1`–`5` | Sort by column (exits semantic mode) |
| `s` | Start a new semantic search |

## CLI Reference

### `tclaude conv index-embeddings`

Build or update the semantic search index.

```bash
tclaude conv index-embeddings          # index current project
tclaude conv index-embeddings -g       # index all projects
tclaude conv index-embeddings --reindex  # force full rebuild
tclaude conv index-embeddings --model mxbai-embed-large  # use a different model
```

| Flag | Description |
|------|-------------|
| `-g, --global` | Index conversations from all projects |
| `--reindex` | Wipe and rebuild all embeddings |
| `--model` | Embedding model name (default: `nomic-embed-text`) |
| `--url` | Ollama API base URL (default: `http://localhost:11434`) |

### `tclaude conv search-embeddings`

Search conversations by meaning.

```bash
tclaude conv search-embeddings "query"
tclaude conv sem "query"          # shorthand alias
```

| Flag | Description |
|------|-------------|
| `-g, --global` | Search across all projects |
| `-n` | Number of results (default: 10) |
| `-l, --long` | Show matching chunk text |
| `--json` | JSON output |
| `--model` | Embedding model name (default: `nomic-embed-text`) |
| `--url` | Ollama API base URL (default: `http://localhost:11434`) |

## How It Works

### Chunking

Each conversation is split into chunks for embedding:

1. **Metadata chunk** (always generated) — title, summary, first prompt, and project path. This is the "what was this conversation about" anchor.
2. **Content chunks** — user + assistant message pairs, grouped at turn boundaries. Chunks target ~24K characters (~8K tokens). Short conversations produce a single content chunk.

### Ranking

Conversations are ranked by the **maximum similarity** across all their chunks, not the sum or average. This prevents long conversations (many chunks) from being artificially boosted over short ones.

### Storage

Embeddings are stored in `~/.tclaude/db.sqlite` (table `conv_embeddings`). Each chunk stores its text, embedding vector (768 floats as raw bytes), and creation timestamp.

### Invalidation

Indexing is incremental using file modification times:

- If a conversation file is newer than its stored embeddings, it gets re-indexed
- Orphaned embeddings (deleted conversations) are cleaned up automatically
- Use `--reindex` to force a full rebuild (useful when switching models)

### Context length handling

If a chunk exceeds the model's context window, `tclaude` automatically reduces it by 1/3 and retries, repeating until it fits. In practice, the 24K character limit rarely triggers this with `nomic-embed-text`'s 8K token context.

## Code Layout

| File | Purpose |
|------|---------|
| `pkg/claude/conv/embeddings.go` | Ollama client, chunking, cosine similarity |
| `pkg/claude/conv/embed_search.go` | CLI commands (`index-embeddings`, `search-embeddings`) |
| `pkg/claude/conv/watch.go` | Interactive watch mode integration |
| `pkg/claude/common/db/embeddings.go` | SQLite storage for embeddings |
| `pkg/claude/conv/embeddings_test.go` | Unit tests with mocked Ollama server |
