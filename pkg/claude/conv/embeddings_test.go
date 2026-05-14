package conv

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// fakeEmbedding returns a deterministic embedding based on the text length.
// Different texts produce different vectors for testing similarity ranking.
func fakeEmbedding(text string, dims int) []float32 {
	vec := make([]float32, dims)
	for i := range vec {
		vec[i] = float32(len(text)%100) / 100.0 * float32(i%10) / 10.0
	}
	// Normalize
	var norm float32
	for _, v := range vec {
		norm += v * v
	}
	if norm > 0 {
		s := float32(math.Sqrt(float64(norm)))
		for i := range vec {
			vec[i] /= s
		}
	}
	return vec
}

// newMockOllama creates a test server that returns fake embeddings.
// contextLimit controls max input length (0 = no limit).
func newMockOllama(t *testing.T, dims int, contextLimit int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.NotFound(w, r)
			return
		}

		var req ollamaEmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		var texts []string
		switch v := req.Input.(type) {
		case string:
			texts = []string{v}
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					texts = append(texts, s)
				}
			}
		}

		// Simulate context length error
		for _, text := range texts {
			if contextLimit > 0 && len(text) > contextLimit {
				w.WriteHeader(http.StatusOK) // Ollama returns 200 with error in body, or 400
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error": "the input length exceeds the context length",
				})
				return
			}
		}

		var embeddings [][]float32
		for _, text := range texts {
			embeddings = append(embeddings, fakeEmbedding(text, dims))
		}

		_ = json.NewEncoder(w).Encode(ollamaEmbedResponse{
			Embeddings:      embeddings,
			PromptEvalCount: len(texts[0]) / 4,
		})
	}))
}

func setupEmbeddingsTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()
}

// --- Float32/Byte conversion tests ---

func TestFloat32ToBytes_RoundTrip(t *testing.T) {
	original := []float32{0.1, -0.5, 3.14, 0, -1.0}
	bytes := Float32ToBytes(original)
	require.Len(t, bytes, len(original)*4, "expected %d bytes", len(original)*4)

	result := BytesToFloat32(bytes)
	require.Len(t, result, len(original), "expected %d floats", len(original))

	for i := range original {
		assert.Equal(t, original[i], result[i], "index %d", i)
	}
}

func TestFloat32ToBytes_Empty(t *testing.T) {
	bytes := Float32ToBytes(nil)
	result := BytesToFloat32(bytes)
	assert.Empty(t, result, "expected 0 floats")
}

// --- Cosine similarity tests ---

func TestCosineSimilarity_Identical(t *testing.T) {
	a := []float32{1, 2, 3}
	sim := CosineSimilarity(a, a)
	assert.InDelta(t, 1.0, sim, 0.0001, "expected ~1.0 for identical vectors")
}

func TestCosineSimilarity_Orthogonal(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{0, 1, 0}
	sim := CosineSimilarity(a, b)
	assert.InDelta(t, 0.0, sim, 0.0001, "expected ~0.0 for orthogonal vectors")
}

func TestCosineSimilarity_Opposite(t *testing.T) {
	a := []float32{1, 2, 3}
	b := []float32{-1, -2, -3}
	sim := CosineSimilarity(a, b)
	assert.InDelta(t, -1.0, sim, 0.0001, "expected ~-1.0 for opposite vectors")
}

func TestCosineSimilarity_DifferentLengths(t *testing.T) {
	a := []float32{1, 2}
	b := []float32{1, 2, 3}
	sim := CosineSimilarity(a, b)
	assert.Equal(t, float32(0), sim, "expected 0 for different length vectors")
}

func TestCosineSimilarity_ZeroVector(t *testing.T) {
	a := []float32{0, 0, 0}
	b := []float32{1, 2, 3}
	sim := CosineSimilarity(a, b)
	assert.Equal(t, float32(0), sim, "expected 0 for zero vector")
}

// --- Chunking tests ---

func writeTestConversation(t *testing.T, dir, sessionID, content string) string {
	t.Helper()
	filePath := filepath.Join(dir, sessionID+".jsonl")
	require.NoError(t, os.WriteFile(filePath, []byte(content), 0644), "Failed to write test file")
	return filePath
}

func TestChunkConversation_MetadataOnly(t *testing.T) {
	entry := SessionEntry{
		SessionID:   "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		CustomTitle: "My Title",
		Summary:     "A summary of the conversation",
		FirstPrompt: "Please help me",
		ProjectPath: "/home/user/project",
		FullPath:    "/nonexistent/path.jsonl", // no content file
	}

	chunks, err := ChunkConversation(entry)
	require.NoError(t, err)
	require.NotEmpty(t, chunks, "expected at least metadata chunk")

	meta := chunks[0]
	assert.Equal(t, "metadata", meta.Type)
	assert.Equal(t, 0, meta.Index)
	assert.True(t, contains(meta.Text, "My Title"), "metadata chunk missing title")
	assert.True(t, contains(meta.Text, "A summary"), "metadata chunk missing summary")
	assert.True(t, contains(meta.Text, "Please help me"), "metadata chunk missing first prompt")
}

func TestChunkConversation_WithContent(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	content := `{"type":"user","message":{"role":"user","content":"How do I fix this bug?"},"timestamp":"2026-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"You need to check the error handling."},"timestamp":"2026-01-01T00:00:01Z"}
{"type":"user","message":{"role":"user","content":"That worked, thanks!"},"timestamp":"2026-01-01T00:00:02Z"}
{"type":"assistant","message":{"role":"assistant","content":"You're welcome!"},"timestamp":"2026-01-01T00:00:03Z"}
`
	filePath := writeTestConversation(t, tmpDir, sessionID, content)

	entry := SessionEntry{
		SessionID:   sessionID,
		FirstPrompt: "How do I fix this bug?",
		FullPath:    filePath,
	}

	chunks, err := ChunkConversation(entry)
	require.NoError(t, err)

	// Should have metadata + 1 content chunk (small conversation)
	require.Len(t, chunks, 2, "expected 2 chunks")

	assert.Equal(t, "metadata", chunks[0].Type, "chunk 0 type")
	assert.Equal(t, "content", chunks[1].Type, "chunk 1 type")
	assert.True(t, contains(chunks[1].Text, "fix this bug"), "content chunk missing user message")
	assert.True(t, contains(chunks[1].Text, "error handling"), "content chunk missing assistant message")
}

func TestChunkConversation_FiltersSystemMessages(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	content := `{"type":"user","message":{"role":"user","content":"<local-command-caveat>system stuff</local-command-caveat>"},"timestamp":"2026-01-01T00:00:00Z"}
{"type":"user","message":{"role":"user","content":"[Request interrupted by user]"},"timestamp":"2026-01-01T00:00:01Z"}
{"type":"user","message":{"role":"user","content":"Real user message"},"timestamp":"2026-01-01T00:00:02Z"}
{"type":"assistant","message":{"role":"assistant","content":"Real response"},"timestamp":"2026-01-01T00:00:03Z"}
`
	filePath := writeTestConversation(t, tmpDir, sessionID, content)

	entry := SessionEntry{
		SessionID:   sessionID,
		FirstPrompt: "Real user message",
		FullPath:    filePath,
	}

	chunks, err := ChunkConversation(entry)
	require.NoError(t, err)

	// Find the content chunk
	var contentChunk *Chunk
	for i := range chunks {
		if chunks[i].Type == "content" {
			contentChunk = &chunks[i]
			break
		}
	}

	require.NotNil(t, contentChunk, "no content chunk found")
	assert.False(t, contains(contentChunk.Text, "local-command-caveat"), "content chunk should not contain system-injected messages")
	assert.False(t, contains(contentChunk.Text, "Request interrupted"), "content chunk should not contain interrupted request messages")
	assert.True(t, contains(contentChunk.Text, "Real user message"), "content chunk missing real user message")
}

func TestChunkConversation_SplitsLongContent(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Generate a conversation that exceeds maxChunkChars
	var lines string
	for i := 0; i < 200; i++ {
		// ~200 chars per turn pair, 200 pairs = ~40K chars of content
		lines += `{"type":"user","message":{"role":"user","content":"This is user message number ` +
			string(rune('A'+i%26)) + ` with some padding text to make it longer and more realistic for testing."},"timestamp":"2026-01-01T00:00:00Z"}` + "\n"
		lines += `{"type":"assistant","message":{"role":"assistant","content":"This is the assistant response to message ` +
			string(rune('A'+i%26)) + ` with enough text to push the total size over the chunk limit boundary."},"timestamp":"2026-01-01T00:00:01Z"}` + "\n"
	}

	filePath := writeTestConversation(t, tmpDir, sessionID, lines)

	entry := SessionEntry{
		SessionID:   sessionID,
		FirstPrompt: "test",
		FullPath:    filePath,
	}

	chunks, err := ChunkConversation(entry)
	require.NoError(t, err)

	// Should have metadata + multiple content chunks
	contentChunks := 0
	for _, c := range chunks {
		if c.Type == "content" {
			contentChunks++
			assert.LessOrEqual(t, len(c.Text), maxChunkChars+1000, "content chunk too large (limit %d)", maxChunkChars)
		}
	}
	assert.GreaterOrEqual(t, contentChunks, 2, "expected multiple content chunks for large conversation")
}

// --- OllamaClient tests with mock server ---

func TestEmbedOne_Success(t *testing.T) {
	server := newMockOllama(t, 768, 0)
	defer server.Close()

	client := NewOllamaClient(server.URL, "test-model")
	emb, err := client.EmbedOne("hello world")
	require.NoError(t, err)
	require.Len(t, emb, 768, "expected 768 dimensions")
}

func TestEmbedOne_ReducesOnContextLengthError(t *testing.T) {
	// Server rejects inputs over 9000 chars
	server := newMockOllama(t, 4, 9000)
	defer server.Close()

	client := NewOllamaClient(server.URL, "test-model")

	// Send 20000 chars — should be reduced by 1/3 repeatedly until under 9000
	// 20000 -> 13333 -> 8888 (fits! under 9000)
	longText := make([]byte, 20000)
	for i := range longText {
		longText[i] = 'a'
	}

	emb, err := client.EmbedOne(string(longText))
	require.NoError(t, err)
	require.Len(t, emb, 4, "expected 4 dimensions")
}

func TestEmbedOne_GivesUpWhenTooSmall(t *testing.T) {
	// Server rejects everything (limit of 1 char)
	server := newMockOllama(t, 4, 1)
	defer server.Close()

	client := NewOllamaClient(server.URL, "test-model")

	// 9000 chars — will reduce until under 8000, then give up
	text := make([]byte, 9000)
	for i := range text {
		text[i] = 'x'
	}

	_, err := client.EmbedOne(string(text))
	require.Error(t, err, "expected error when text can't be reduced enough")
}

func TestEmbedOne_ConnectionError(t *testing.T) {
	client := NewOllamaClient("http://localhost:1", "test-model")
	_, err := client.EmbedOne("hello")
	require.Error(t, err, "expected error for unreachable server")
}

// --- IndexConversation with mock ---

func TestIndexConversation_StoresEmbeddings(t *testing.T) {
	setupEmbeddingsTestDB(t)
	server := newMockOllama(t, 8, 0)
	defer server.Close()

	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	content := `{"type":"user","message":{"role":"user","content":"hello"},"timestamp":"2026-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"hi there"},"timestamp":"2026-01-01T00:00:01Z"}
`
	filePath := writeTestConversation(t, tmpDir, sessionID, content)

	entry := SessionEntry{
		SessionID:   sessionID,
		FirstPrompt: "hello",
		FullPath:    filePath,
	}

	client := NewOllamaClient(server.URL, "test-model")
	chunks, err := IndexConversation(entry, client)
	require.NoError(t, err)
	require.NotZero(t, chunks, "expected at least 1 chunk stored")

	// Verify stored in DB
	rows, err := db.ListEmbeddingsForConv(sessionID)
	require.NoError(t, err, "db error")
	assert.Equal(t, chunks, len(rows), "expected %d rows in DB", chunks)
	for _, row := range rows {
		assert.Equal(t, "test-model", row.Model)
		emb := BytesToFloat32(row.Embedding)
		assert.Len(t, emb, 8, "expected 8 dimensions")
	}
}

func TestIndexConversation_ReplacesOldEmbeddings(t *testing.T) {
	setupEmbeddingsTestDB(t)
	server := newMockOllama(t, 4, 0)
	defer server.Close()

	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	content := `{"type":"user","message":{"role":"user","content":"first"},"timestamp":"2026-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"response"},"timestamp":"2026-01-01T00:00:01Z"}
`
	filePath := writeTestConversation(t, tmpDir, sessionID, content)
	entry := SessionEntry{
		SessionID:   sessionID,
		FirstPrompt: "first",
		FullPath:    filePath,
	}

	client := NewOllamaClient(server.URL, "test-model")

	// Index once
	chunks1, err := IndexConversation(entry, client)
	require.NoError(t, err, "first index")

	// Index again (simulating re-index)
	chunks2, err := IndexConversation(entry, client)
	require.NoError(t, err, "second index")

	assert.Equal(t, chunks1, chunks2, "chunk count changed")

	// Should have same number of rows, not double
	rows, err := db.ListEmbeddingsForConv(sessionID)
	require.NoError(t, err, "db error")
	assert.Equal(t, chunks2, len(rows), "expected %d rows after re-index", chunks2)
}

// --- SearchEmbeddings tests ---

func TestSearchEmbeddings_RanksByBestChunk(t *testing.T) {
	setupEmbeddingsTestDB(t)

	// Manually store embeddings for two conversations with known vectors
	convA := "conv-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	convB := "conv-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	// Query vector
	queryVec := []float32{1, 0, 0, 0}

	// Conv A: chunk closely aligned with query
	vecA := []float32{0.9, 0.1, 0, 0}
	db.UpsertEmbedding(&db.EmbeddingRow{
		ConvID: convA, ChunkIndex: 0, ChunkType: "metadata",
		ChunkText: "auth conversation", Embedding: Float32ToBytes(vecA), Model: "test",
	})

	// Conv B: chunk less aligned
	vecB := []float32{0.1, 0.9, 0, 0}
	db.UpsertEmbedding(&db.EmbeddingRow{
		ConvID: convB, ChunkIndex: 0, ChunkType: "metadata",
		ChunkText: "unrelated conversation", Embedding: Float32ToBytes(vecB), Model: "test",
	})

	entries := []SessionEntry{
		{SessionID: convA, FirstPrompt: "auth stuff"},
		{SessionID: convB, FirstPrompt: "other stuff"},
	}

	results, err := SearchEmbeddings(queryVec, entries, 10)
	require.NoError(t, err, "search error")

	require.Len(t, results, 2, "expected 2 results")

	// Conv A should rank first (higher similarity to query)
	assert.Equal(t, convA, results[0].Entry.SessionID, "expected conv A first")
	assert.Greater(t, results[0].Similarity, results[1].Similarity, "expected first result to have higher similarity")
}

func TestSearchEmbeddings_TopKLimit(t *testing.T) {
	setupEmbeddingsTestDB(t)

	// Store 5 conversations
	var entries []SessionEntry
	for i := 0; i < 5; i++ {
		convID := "conv-" + string(rune('a'+i)) + "aaa-aaaa-aaaa-aaaaaaaaaaaa"
		vec := make([]float32, 4)
		vec[0] = float32(i) / 5.0
		db.UpsertEmbedding(&db.EmbeddingRow{
			ConvID: convID, ChunkIndex: 0, ChunkType: "metadata",
			ChunkText: "test", Embedding: Float32ToBytes(vec), Model: "test",
		})
		entries = append(entries, SessionEntry{SessionID: convID})
	}

	queryVec := []float32{1, 0, 0, 0}
	results, err := SearchEmbeddings(queryVec, entries, 3)
	require.NoError(t, err, "search error")
	assert.Len(t, results, 3, "expected 3 results with topK=3")
}

func TestSearchEmbeddings_UsesMaxSimilarityAcrossChunks(t *testing.T) {
	setupEmbeddingsTestDB(t)

	convID := "conv-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	queryVec := []float32{1, 0, 0, 0}

	// Chunk 0: low similarity
	vecLow := []float32{0.1, 0.9, 0, 0}
	db.UpsertEmbedding(&db.EmbeddingRow{
		ConvID: convID, ChunkIndex: 0, ChunkType: "metadata",
		ChunkText: "low match", Embedding: Float32ToBytes(vecLow), Model: "test",
	})

	// Chunk 1: high similarity
	vecHigh := []float32{0.95, 0.05, 0, 0}
	db.UpsertEmbedding(&db.EmbeddingRow{
		ConvID: convID, ChunkIndex: 1, ChunkType: "content",
		ChunkText: "high match", Embedding: Float32ToBytes(vecHigh), Model: "test",
	})

	entries := []SessionEntry{{SessionID: convID, FirstPrompt: "test"}}

	results, err := SearchEmbeddings(queryVec, entries, 10)
	require.NoError(t, err, "search error")

	require.Len(t, results, 1, "expected 1 result")

	// Should use the high-similarity chunk
	assert.Equal(t, "high match", results[0].ChunkText)
	assert.Equal(t, "content", results[0].ChunkType)
}

func TestSearchEmbeddings_NoResults(t *testing.T) {
	setupEmbeddingsTestDB(t)

	queryVec := []float32{1, 0, 0, 0}
	results, err := SearchEmbeddings(queryVec, nil, 10)
	require.NoError(t, err)
	assert.Nil(t, results, "expected nil results")
}

// --- Orphan cleanup scoping tests ---

// TestRunIndexEmbeddings_LocalModePreservesOtherProjectEmbeddings verifies that
// running index-embeddings in local (non-global) mode does NOT delete embeddings
// belonging to conversations in other projects.
func TestRunIndexEmbeddings_LocalModePreservesOtherProjectEmbeddings(t *testing.T) {
	setupEmbeddingsTestDB(t)
	server := newMockOllama(t, 8, 0)
	defer server.Close()

	home := os.Getenv("HOME")

	// Project A: the "local" project we'll run index-embeddings on.
	// It has convA1 (still on disk) and convA2 (orphan — indexed but file deleted).
	// Project B: a different project with convB whose embeddings must survive.
	projectDirB := filepath.Join(home, ".claude", "projects", "-projectB")
	os.MkdirAll(projectDirB, 0755)

	convA1 := "aaaaaaaa-aaaa-aaaa-aaaa-111111111111"
	convA2 := "aaaaaaaa-aaaa-aaaa-aaaa-222222222222"
	convB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	convContent := `{"type":"user","cwd":"/x","message":{"role":"user","content":"hello"},"timestamp":"2026-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"hi"},"timestamp":"2026-01-01T00:00:01Z"}
`
	os.WriteFile(filepath.Join(projectDirB, convB+".jsonl"), []byte(convContent), 0644)

	// Create a working directory that maps to project A.
	// Resolve symlinks since os.Getwd() returns the real path on macOS
	// (e.g. /private/var/... vs /var/...) and GetClaudeProjectPath must match.
	cwdA, _ := filepath.EvalSymlinks(t.TempDir())
	projectDirA := GetClaudeProjectPath(cwdA)
	os.MkdirAll(projectDirA, 0755)
	// convA1 exists on disk, convA2 does not (orphan)
	os.WriteFile(filepath.Join(projectDirA, convA1+".jsonl"), []byte(convContent), 0644)

	// Index all three conversations (store embeddings)
	client := NewOllamaClient(server.URL, "test-model")
	for _, e := range []struct {
		id, dir string
	}{
		{convA1, projectDirA},
		{convA2, projectDirA},
		{convB, projectDirB},
	} {
		// For convA2 (orphan), write a temp file so IndexConversation can read it
		fullPath := filepath.Join(e.dir, e.id+".jsonl")
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			tmp := filepath.Join(t.TempDir(), e.id+".jsonl")
			os.WriteFile(tmp, []byte(convContent), 0644)
			fullPath = tmp
		}
		entry := SessionEntry{SessionID: e.id, FirstPrompt: "hello", FullPath: fullPath}
		_, err := IndexConversation(entry, client)
		require.NoError(t, err, "index %s", e.id)
		// Insert conv_index entry so ListEmbeddedConvIDsForProject can join
		db.UpsertConvIndex(&db.ConvIndexRow{
			ConvID:     e.id,
			ProjectDir: e.dir,
			FullPath:   filepath.Join(e.dir, e.id+".jsonl"),
		})
	}

	// Verify all three have embeddings
	for _, id := range []string{convA1, convA2, convB} {
		rows, _ := db.ListEmbeddingsForConv(id)
		require.NotEmpty(t, rows, "setup failed: %s has no embeddings", id)
	}

	// Run index-embeddings in local mode (cwd = cwdA -> projectDirA)
	oldWd, _ := os.Getwd()
	os.Chdir(cwdA)
	defer os.Chdir(oldWd)

	stdout, _ := os.CreateTemp(t.TempDir(), "stdout")
	stderr, _ := os.CreateTemp(t.TempDir(), "stderr")

	params := &IndexEmbeddingsParams{
		Global: false,
		Model:  "test-model",
		URL:    server.URL,
	}
	RunIndexEmbeddings(params, stdout, stderr)

	// convA1: still on disk, embeddings should survive
	rowsA1, _ := db.ListEmbeddingsForConv(convA1)
	assert.NotEmpty(t, rowsA1, "convA1 embeddings were incorrectly deleted")

	// convA2: orphan within project A, should be cleaned up
	rowsA2, _ := db.ListEmbeddingsForConv(convA2)
	assert.Empty(t, rowsA2, "expected convA2 (orphan) embeddings to be cleaned up")

	// convB: belongs to project B, MUST still exist
	rowsB, _ := db.ListEmbeddingsForConv(convB)
	assert.NotEmpty(t, rowsB, "convB embeddings were incorrectly deleted by local-mode orphan cleanup")
}

// TestRunIndexEmbeddings_GlobalModeCleanupOrphans verifies that global mode
// correctly cleans up orphaned embeddings across all projects.
func TestRunIndexEmbeddings_GlobalModeCleanupOrphans(t *testing.T) {
	setupEmbeddingsTestDB(t)
	server := newMockOllama(t, 8, 0)
	defer server.Close()

	home := os.Getenv("HOME")

	// Set up a project with a conversation
	projectDir := filepath.Join(home, ".claude", "projects", "-testproject")
	os.MkdirAll(projectDir, 0755)

	convLive := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	convOrphan := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	convContent := `{"type":"user","cwd":"/x","message":{"role":"user","content":"hello"},"timestamp":"2026-01-01T00:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"hi"},"timestamp":"2026-01-01T00:00:01Z"}
`
	// Only the live conversation has a .jsonl file
	os.WriteFile(filepath.Join(projectDir, convLive+".jsonl"), []byte(convContent), 0644)

	// Index both conversations in DB
	client := NewOllamaClient(server.URL, "test-model")
	entryLive := SessionEntry{SessionID: convLive, FirstPrompt: "hello", FullPath: filepath.Join(projectDir, convLive+".jsonl")}
	entryOrphan := SessionEntry{SessionID: convOrphan, FirstPrompt: "hello", FullPath: filepath.Join(projectDir, convOrphan+".jsonl")}

	// Write a temp file for the orphan so IndexConversation can read it
	tmpOrphanFile := filepath.Join(t.TempDir(), convOrphan+".jsonl")
	os.WriteFile(tmpOrphanFile, []byte(convContent), 0644)
	entryOrphan.FullPath = tmpOrphanFile

	_, err := IndexConversation(entryLive, client)
	require.NoError(t, err, "index live")
	_, err = IndexConversation(entryOrphan, client)
	require.NoError(t, err, "index orphan")

	// Verify both have embeddings
	rowsLive, _ := db.ListEmbeddingsForConv(convLive)
	rowsOrphan, _ := db.ListEmbeddingsForConv(convOrphan)
	require.NotEmpty(t, rowsLive, "setup failed: live has no embeddings")
	require.NotEmpty(t, rowsOrphan, "setup failed: orphan has no embeddings")

	// Run in global mode — orphan has no .jsonl file so should be cleaned up
	stdout, _ := os.CreateTemp(t.TempDir(), "stdout")
	stderr, _ := os.CreateTemp(t.TempDir(), "stderr")

	params := &IndexEmbeddingsParams{
		Global: true,
		Model:  "test-model",
		URL:    server.URL,
	}
	RunIndexEmbeddings(params, stdout, stderr)

	// Live conversation's embeddings should still exist
	rowsLive, _ = db.ListEmbeddingsForConv(convLive)
	assert.NotEmpty(t, rowsLive, "live conversation embeddings were incorrectly deleted")

	// Orphan's embeddings should be cleaned up
	rowsOrphan, _ = db.ListEmbeddingsForConv(convOrphan)
	assert.Empty(t, rowsOrphan, "expected orphan embeddings to be cleaned up")
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
