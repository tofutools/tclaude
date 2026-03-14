package conv

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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
				json.NewEncoder(w).Encode(map[string]string{
					"error": "the input length exceeds the context length",
				})
				return
			}
		}

		var embeddings [][]float32
		for _, text := range texts {
			embeddings = append(embeddings, fakeEmbedding(text, dims))
		}

		json.NewEncoder(w).Encode(ollamaEmbedResponse{
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
	if len(bytes) != len(original)*4 {
		t.Fatalf("expected %d bytes, got %d", len(original)*4, len(bytes))
	}

	result := BytesToFloat32(bytes)
	if len(result) != len(original) {
		t.Fatalf("expected %d floats, got %d", len(original), len(result))
	}

	for i := range original {
		if result[i] != original[i] {
			t.Errorf("index %d: expected %f, got %f", i, original[i], result[i])
		}
	}
}

func TestFloat32ToBytes_Empty(t *testing.T) {
	bytes := Float32ToBytes(nil)
	result := BytesToFloat32(bytes)
	if len(result) != 0 {
		t.Fatalf("expected 0 floats, got %d", len(result))
	}
}

// --- Cosine similarity tests ---

func TestCosineSimilarity_Identical(t *testing.T) {
	a := []float32{1, 2, 3}
	sim := CosineSimilarity(a, a)
	if math.Abs(float64(sim)-1.0) > 0.0001 {
		t.Errorf("expected ~1.0 for identical vectors, got %f", sim)
	}
}

func TestCosineSimilarity_Orthogonal(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{0, 1, 0}
	sim := CosineSimilarity(a, b)
	if math.Abs(float64(sim)) > 0.0001 {
		t.Errorf("expected ~0.0 for orthogonal vectors, got %f", sim)
	}
}

func TestCosineSimilarity_Opposite(t *testing.T) {
	a := []float32{1, 2, 3}
	b := []float32{-1, -2, -3}
	sim := CosineSimilarity(a, b)
	if math.Abs(float64(sim)+1.0) > 0.0001 {
		t.Errorf("expected ~-1.0 for opposite vectors, got %f", sim)
	}
}

func TestCosineSimilarity_DifferentLengths(t *testing.T) {
	a := []float32{1, 2}
	b := []float32{1, 2, 3}
	sim := CosineSimilarity(a, b)
	if sim != 0 {
		t.Errorf("expected 0 for different length vectors, got %f", sim)
	}
}

func TestCosineSimilarity_ZeroVector(t *testing.T) {
	a := []float32{0, 0, 0}
	b := []float32{1, 2, 3}
	sim := CosineSimilarity(a, b)
	if sim != 0 {
		t.Errorf("expected 0 for zero vector, got %f", sim)
	}
}

// --- Chunking tests ---

func writeTestConversation(t *testing.T, dir, sessionID, content string) string {
	t.Helper()
	filePath := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least metadata chunk")
	}

	meta := chunks[0]
	if meta.Type != "metadata" {
		t.Errorf("expected type 'metadata', got %q", meta.Type)
	}
	if meta.Index != 0 {
		t.Errorf("expected index 0, got %d", meta.Index)
	}
	if !contains(meta.Text, "My Title") {
		t.Error("metadata chunk missing title")
	}
	if !contains(meta.Text, "A summary") {
		t.Error("metadata chunk missing summary")
	}
	if !contains(meta.Text, "Please help me") {
		t.Error("metadata chunk missing first prompt")
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have metadata + 1 content chunk (small conversation)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}

	if chunks[0].Type != "metadata" {
		t.Errorf("chunk 0: expected type 'metadata', got %q", chunks[0].Type)
	}
	if chunks[1].Type != "content" {
		t.Errorf("chunk 1: expected type 'content', got %q", chunks[1].Type)
	}
	if !contains(chunks[1].Text, "fix this bug") {
		t.Error("content chunk missing user message")
	}
	if !contains(chunks[1].Text, "error handling") {
		t.Error("content chunk missing assistant message")
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the content chunk
	var contentChunk *Chunk
	for i := range chunks {
		if chunks[i].Type == "content" {
			contentChunk = &chunks[i]
			break
		}
	}

	if contentChunk == nil {
		t.Fatal("no content chunk found")
	}
	if contains(contentChunk.Text, "local-command-caveat") {
		t.Error("content chunk should not contain system-injected messages")
	}
	if contains(contentChunk.Text, "Request interrupted") {
		t.Error("content chunk should not contain interrupted request messages")
	}
	if !contains(contentChunk.Text, "Real user message") {
		t.Error("content chunk missing real user message")
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should have metadata + multiple content chunks
	contentChunks := 0
	for _, c := range chunks {
		if c.Type == "content" {
			contentChunks++
			if len(c.Text) > maxChunkChars+1000 { // some slack for the last turn
				t.Errorf("content chunk too large: %d chars (limit %d)", len(c.Text), maxChunkChars)
			}
		}
	}
	if contentChunks < 2 {
		t.Errorf("expected multiple content chunks for large conversation, got %d", contentChunks)
	}
}

// --- OllamaClient tests with mock server ---

func TestEmbedOne_Success(t *testing.T) {
	server := newMockOllama(t, 768, 0)
	defer server.Close()

	client := NewOllamaClient(server.URL, "test-model")
	emb, err := client.EmbedOne("hello world")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(emb) != 768 {
		t.Fatalf("expected 768 dimensions, got %d", len(emb))
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(emb) != 4 {
		t.Fatalf("expected 4 dimensions, got %d", len(emb))
	}
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
	if err == nil {
		t.Fatal("expected error when text can't be reduced enough")
	}
}

func TestEmbedOne_ConnectionError(t *testing.T) {
	client := NewOllamaClient("http://localhost:1", "test-model")
	_, err := client.EmbedOne("hello")
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunks == 0 {
		t.Fatal("expected at least 1 chunk stored")
	}

	// Verify stored in DB
	rows, err := db.ListEmbeddingsForConv(sessionID)
	if err != nil {
		t.Fatalf("db error: %v", err)
	}
	if len(rows) != chunks {
		t.Errorf("expected %d rows in DB, got %d", chunks, len(rows))
	}
	for _, row := range rows {
		if row.Model != "test-model" {
			t.Errorf("expected model 'test-model', got %q", row.Model)
		}
		emb := BytesToFloat32(row.Embedding)
		if len(emb) != 8 {
			t.Errorf("expected 8 dimensions, got %d", len(emb))
		}
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
	if err != nil {
		t.Fatalf("first index: %v", err)
	}

	// Index again (simulating re-index)
	chunks2, err := IndexConversation(entry, client)
	if err != nil {
		t.Fatalf("second index: %v", err)
	}

	if chunks1 != chunks2 {
		t.Errorf("chunk count changed: %d -> %d", chunks1, chunks2)
	}

	// Should have same number of rows, not double
	rows, err := db.ListEmbeddingsForConv(sessionID)
	if err != nil {
		t.Fatalf("db error: %v", err)
	}
	if len(rows) != chunks2 {
		t.Errorf("expected %d rows after re-index, got %d", chunks2, len(rows))
	}
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
	if err != nil {
		t.Fatalf("search error: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Conv A should rank first (higher similarity to query)
	if results[0].Entry.SessionID != convA {
		t.Errorf("expected conv A first, got %s", results[0].Entry.SessionID)
	}
	if results[0].Similarity <= results[1].Similarity {
		t.Errorf("expected first result to have higher similarity: %f <= %f",
			results[0].Similarity, results[1].Similarity)
	}
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
	if err != nil {
		t.Fatalf("search error: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("expected 3 results with topK=3, got %d", len(results))
	}
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
	if err != nil {
		t.Fatalf("search error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	// Should use the high-similarity chunk
	if results[0].ChunkText != "high match" {
		t.Errorf("expected 'high match' chunk, got %q", results[0].ChunkText)
	}
	if results[0].ChunkType != "content" {
		t.Errorf("expected chunk type 'content', got %q", results[0].ChunkType)
	}
}

func TestSearchEmbeddings_NoResults(t *testing.T) {
	setupEmbeddingsTestDB(t)

	queryVec := []float32{1, 0, 0, 0}
	results, err := SearchEmbeddings(queryVec, nil, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results, got %d", len(results))
	}
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
