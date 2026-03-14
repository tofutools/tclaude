# Semantic Search

Search conversations by meaning, not just keywords. Powered by local embeddings via [Ollama](https://ollama.com) — your data never leaves your machine.

## Why Semantic Search?

When searching past conversations, you rarely remember exact keywords — you remember the *subject* or *concept* you discussed. Keyword search (`conv search`) requires exact text matches, which often misses what you're looking for.

Semantic search embeds your conversations into vector space and finds matches by meaning. Searching for "auth token refresh" will find conversations about authentication and OAuth even if those exact words weren't used.

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

### 3. Pull an embedding model

```bash
# Default — best quality, 32K context, 100+ languages
ollama pull qwen3-embedding:0.6b

# Alternative — faster indexing, smaller download
ollama pull nomic-embed-text
```

| Model | Size | Dimensions | Context | Indexing speed | Languages |
|-------|------|-----------|---------|----------------|-----------|
| `qwen3-embedding:0.6b` (default) | 639 MB | 1024 | 32K tokens | ~1.5s/conv | 100+ |
| `nomic-embed-text` | 274 MB | 768 | 8K tokens | ~0.1s/conv | English-focused |

`qwen3-embedding:0.6b` produces higher-quality embeddings with a much larger context window (most conversations fit in a single chunk). `nomic-embed-text` is significantly faster to index and uses less disk/memory — a good choice if you have many conversations or limited hardware.

To use nomic instead of the default:

```bash
export TCLAUDE_EMBED_MODEL=nomic-embed-text
```

Or pass `--model nomic-embed-text` to individual commands.

### 4. Verify

```bash
curl -s http://localhost:11434/api/embed \
  -d '{"model": "qwen3-embedding:0.6b", "input": "hello world"}' \
  | python3 -c "import sys,json; e=json.load(sys.stdin)['embeddings'][0]; print(f'{len(e)} dimensions')"
# Expected: 1024 dimensions
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
| `--model` | Embedding model name (default: `qwen3-embedding:0.6b`) |
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
| `--model` | Embedding model name (default: `qwen3-embedding:0.6b`) |
| `--url` | Ollama API base URL (default: `http://localhost:11434`) |

## How It Works

### Chunking

Each conversation is split into chunks for embedding:

1. **Metadata chunk** (always generated) — title, summary, first prompt, and project path. This is the "what was this conversation about" anchor.
2. **Content chunks** — user + assistant message pairs, grouped at turn boundaries. Chunks target ~24K characters (~8K tokens). Short conversations produce a single content chunk.

### Ranking

Conversations are ranked by the **maximum similarity** across all their chunks, not the sum or average. This prevents long conversations (many chunks) from being artificially boosted over short ones.

### Storage

Embeddings are stored in `~/.tclaude/db.sqlite` (table `conv_embeddings`). Each chunk stores its text, embedding vector (raw float32 bytes — 1024 dims for qwen3-embedding, 768 for nomic), model name, and creation timestamp.

### Invalidation

Indexing is incremental using file modification times:

- If a conversation file is newer than its stored embeddings, it gets re-indexed
- Orphaned embeddings (deleted conversations) are cleaned up automatically
- Use `--reindex` to force a full rebuild (useful when switching models)

### Context length handling

If a chunk exceeds the model's context window, `tclaude` automatically reduces it by 1/3 and retries, repeating until it fits. In practice, this rarely triggers — especially with `qwen3-embedding`'s 32K token context.

!!! note "Switching models"
    Embeddings are model-specific — you can't search with a different model than you indexed with. If you switch models, rebuild the index with `tclaude conv index-embeddings --reindex`.

## Code Layout

| File | Purpose |
|------|---------|
| `pkg/claude/conv/embeddings.go` | Ollama client, chunking, cosine similarity |
| `pkg/claude/conv/embed_search.go` | CLI commands (`index-embeddings`, `search-embeddings`) |
| `pkg/claude/conv/watch.go` | Interactive watch mode integration |
| `pkg/claude/common/db/embeddings.go` | SQLite storage for embeddings |
| `pkg/claude/conv/embeddings_test.go` | Unit tests with mocked Ollama server |
