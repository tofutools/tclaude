# Semantic Search for Claude Code Sessions

## Problem

When searching past conversations, users rarely remember exact keywords — they remember
the *subject* or *concept* they discussed. Regex/keyword search fails here. The existing
AI search (`tclaude conv ai-search`) works but is slow, expensive, and sends truncated
data (200 char prompts) to Claude.

## Approach

Local semantic search using embedding vectors. Conversations are chunked, embedded via a
local model (Ollama), stored in SQLite, and searched by cosine similarity.

### Why local embeddings?

- **Privacy**: conversation data never leaves the machine.
- **Speed**: sub-second search once indexed, vs seconds for API-based AI search.
- **Cost**: zero per-query cost after initial setup.
- **Offline**: works without internet after model is downloaded.

## Architecture

### Embedding backend

[Ollama](https://ollama.com) running locally on `localhost:11434` (loopback only, not
exposed on external interfaces). The `/api/embed` endpoint accepts text and returns
embedding vectors. The base URL is configurable via `--url` to support alternative
compatible backends.

**Default model**: `nomic-embed-text` — chosen for:
- 8,192 token context window (important for longer chunks)
- Small footprint: 274MB disk, ~0.5GB RAM (unloads after 5min idle)
- Very fast inference (sub-100ms per embedding on Apple Silicon)
- Good quality (surpasses OpenAI ada-002 on short and long context tasks)

### Chunking strategy

Each conversation produces multiple chunks:

1. **Metadata chunk** (index 0) — `title + summary + first prompt + project path`.
   Always generated. This is the "what was this conversation about" anchor.

2. **Content chunks** — actual message content, split at turn boundaries:
   - Messages are grouped as user+assistant turn pairs to preserve semantic context
     (a question without its answer loses meaning).
   - Turns accumulate into a buffer. When adding the next turn would exceed ~24K chars
     (~8K tokens), the buffer is emitted as a chunk and a new one starts.
   - Short conversations (under the limit) produce a single content chunk.
   - System-injected messages and interrupted request markers are filtered out.

### Adaptive context handling

The Ollama `/api/embed` endpoint returns HTTP 400 with
`{"error": "the input length exceeds the context length"}` when input is too large.

`EmbedOne()` handles this automatically:
- On context length error, reduce text by 1/3 and retry.
- Repeat until it fits or text is under 8K chars (at which point, give up on that chunk).
- Logs a warning via `slog` when reduction occurs.

**Known limitation**: reduction truncates the tail of the chunk — the lost content is not
re-split into a new chunk. This is a future improvement. In practice, the 24K char limit
rarely triggers reduction with `nomic-embed-text`'s 8K token context.

Ollama does not yet offer a `/api/tokenize` endpoint (PR pending), so exact token counting
isn't possible. The char-based estimate works well enough.

### Storage

SQLite table `conv_embeddings` in `~/.tclaude/db.sqlite` (schema v5):

```sql
CREATE TABLE conv_embeddings (
    conv_id     TEXT NOT NULL,
    chunk_index INTEGER NOT NULL,
    chunk_type  TEXT NOT NULL,  -- "metadata" or "content"
    chunk_text  TEXT NOT NULL,
    embedding   BLOB NOT NULL,  -- float32 array as raw bytes
    model       TEXT NOT NULL,  -- e.g. "nomic-embed-text"
    created_at  TEXT NOT NULL,  -- RFC3339
    PRIMARY KEY (conv_id, chunk_index)
);
CREATE INDEX idx_conv_embeddings_conv_id ON conv_embeddings(conv_id);
```

Embeddings are stored as raw `[]float32` bytes (768 floats x 4 bytes = 3,072 bytes per
chunk for nomic-embed-text).

### Search flow

1. Embed the query text via Ollama `/api/embed`.
2. Load all embeddings from SQLite into memory.
3. Compute cosine similarity between query embedding and every stored chunk.
4. Rank conversations by their best-matching chunk (max similarity across chunks).
5. Return top-K conversations with the matching chunk text as context.

Brute-force cosine similarity is fine for personal conversation history scale
(thousands to tens of thousands of chunks — sub-millisecond in memory).

### Invalidation

Same mtime-based pattern as the existing `conv_index`:
- On indexing, compare file mtime against stored `created_at`.
- If the conversation file is newer, re-chunk and re-embed.
- Orphaned embeddings (conversations deleted from disk) are cleaned up on each index run.
- `--reindex` flag wipes all embeddings and rebuilds from scratch (also useful when
  switching to a different embedding model).

## Commands

### `tclaude conv index-embeddings`

Build the semantic search index. Chunks conversations and generates embeddings via Ollama.

```bash
tclaude conv index-embeddings          # index current project
tclaude conv index-embeddings -g       # index all projects
tclaude conv index-embeddings --reindex  # force full rebuild
tclaude conv index-embeddings --model mxbai-embed-large  # use different model
```

Flags:
- `-g, --global` — index conversations from all projects
- `--reindex` — wipe and rebuild all embeddings
- `--model` — embedding model name (default: `nomic-embed-text`)
- `--url` — Ollama API base URL (default: `http://localhost:11434`)

Output shows per-conversation progress with chunk count and timing:
```
Indexing 48 conversations (15 already indexed)...
  [1/48] a9a330a8 please unzip all the zip files - 1 chunks (66ms)
  [2/48] 1211af48 hello - 2 chunks (44ms)
  ...
Done in 5.6s: 48 conversations, 78 chunks, 0 errors
```

### `tclaude conv search-embeddings "query"`

Semantic search across indexed conversations.

```bash
tclaude conv search-embeddings "auth token refresh"
tclaude conv search-embeddings -g "kubernetes deployment" -n 5
tclaude conv search-embeddings -g -l "database migration"  # show matching chunks
tclaude conv search-embeddings --json "API rate limiting"
```

Aliases: `sem`, `semantic`

Flags:
- `-g, --global` — search across all projects
- `-n` — number of results (default: 10)
- `-l, --long` — show matching chunk text
- `--json` — JSON output
- `--model` — embedding model name (default: `nomic-embed-text`)
- `--url` — Ollama API base URL (default: `http://localhost:11434`)

### Phase 2: Interactive integration (future)

- Hotkey in `tclaude conv ls -w` (watch mode) to toggle semantic search.
- Background indexing on watch mode startup.
- Similarity score column in the table.

## Dependencies

- **Runtime**: Ollama installed and running (`ollama serve`).
- **Model**: `ollama pull nomic-embed-text` (one-time, 274MB download).
- **Code**: No new Go dependencies — pure HTTP client to Ollama API, existing SQLite via
  `modernc.org/sqlite`.

## Setup guide

### Install Ollama (macOS)

```bash
# Via Homebrew (recommended for CLI usage — reads shell env vars normally)
brew install ollama

# Start as a background service (auto-starts on login)
brew services start ollama

# Or run manually in foreground
ollama serve
```

Alternative: `curl -fsSL https://ollama.com/install.sh | sh` (installs the macOS app
variant, which auto-starts but requires `launchctl setenv` for env vars).

### Pull the embedding model

```bash
ollama pull nomic-embed-text  # ~274MB download, one-time
```

### Verify it works

```bash
# Test the embed endpoint
curl -s http://localhost:11434/api/embed \
  -d '{"model": "nomic-embed-text", "input": "hello world"}' \
  | python3 -c "import sys,json; e=json.load(sys.stdin)['embeddings'][0]; print(f'{len(e)} dimensions')"
# Expected: 768 dimensions
```

## Ollama API reference

```bash
# Embed text
curl http://localhost:11434/api/embed -d '{
  "model": "nomic-embed-text",
  "input": "your text here"
}'
# Response: {"embeddings": [[0.123, -0.456, ...]], "prompt_eval_count": 5}

# Context length exceeded (HTTP 400):
# {"error": "the input length exceeds the context length"}
```

The `prompt_eval_count` field in successful responses reports the actual token count.

## Verified results (2026-03-14)

Tested on macOS (Apple Silicon, Homebrew install, Ollama 0.18.0):
- Ollama listens on `127.0.0.1:11434` only (loopback, not exposed externally)
- Memory: ~430MB with model loaded, ~42MB server-only (model unloads after 5min idle)
- Embedding endpoint returns 768-dimensional float32 vectors
- Semantic similarity ranking works correctly: query "authentication debugging"
  scored 0.59 against an auth-related text vs 0.37-0.38 against unrelated texts
- 48 conversations indexed globally in ~5.6s, producing 78 chunks with zero errors
- Search queries return results in sub-second time
