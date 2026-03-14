package conv

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	defaultOllamaURL  = "http://localhost:11434"
	defaultEmbedModel = "qwen3-embedding:0.6b"
	maxChunkChars     = 24000 // target ~8K tokens; EmbedOne auto-reduces if too large
)

// OllamaClient calls the Ollama embedding API.
type OllamaClient struct {
	BaseURL string
	Model   string
	Client  *http.Client
}

// NewOllamaClient creates a client with defaults.
func NewOllamaClient(baseURL, model string) *OllamaClient {
	if baseURL == "" {
		baseURL = defaultOllamaURL
	}
	if model == "" {
		model = defaultEmbedModel
	}
	return &OllamaClient{
		BaseURL: baseURL,
		Model:   model,
		Client:  &http.Client{Timeout: 60 * time.Second},
	}
}

type ollamaEmbedRequest struct {
	Model string `json:"model"`
	Input any    `json:"input"` // string or []string
}

type ollamaEmbedResponse struct {
	Embeddings     [][]float32 `json:"embeddings"`
	Error          string      `json:"error,omitempty"`
	PromptEvalCount int        `json:"prompt_eval_count,omitempty"`
}

var errContextLength = fmt.Errorf("input length exceeds context length")

// embedRaw makes a single embed API call without retry logic.
func (c *OllamaClient) embedRaw(input any) (*ollamaEmbedResponse, error) {
	body, err := json.Marshal(ollamaEmbedRequest{Model: c.Model, Input: input})
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}

	resp, err := c.Client.Post(c.BaseURL+"/api/embed", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama request failed (is Ollama running?): %w", err)
	}
	defer resp.Body.Close()

	var result ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("ollama returned status %d (body not JSON)", resp.StatusCode)
		}
		return nil, fmt.Errorf("decode ollama response: %w", err)
	}

	if result.Error != "" {
		if strings.Contains(result.Error, "context length") {
			return nil, errContextLength
		}
		return nil, fmt.Errorf("ollama error: %s", result.Error)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	return &result, nil
}

// EmbedOne embeds a single text, automatically reducing size if it exceeds the model's
// context length. Reduces by 1/3 each attempt until it fits or text is under 8K chars.
// NOTE: reduction means the tail of the text is lost. Future improvement: re-split into
// smaller chunks instead of truncating.
func (c *OllamaClient) EmbedOne(text string) ([]float32, error) {
	originalLen := len(text)
	for {
		result, err := c.embedRaw(text)
		if err == errContextLength && len(text) > 8000 {
			newLen := len(text) * 2 / 3
			slog.Warn("embedding input too large, reducing",
				"original_chars", originalLen,
				"from_chars", len(text),
				"to_chars", newLen)
			text = text[:newLen]
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(result.Embeddings) == 0 {
			return nil, fmt.Errorf("ollama returned no embeddings")
		}
		return result.Embeddings[0], nil
	}
}

// Chunk represents a piece of a conversation to be embedded.
type Chunk struct {
	Index int
	Type  string // "metadata" or "content"
	Text  string
}

// ChunkConversation reads a .jsonl conversation file and returns chunks.
// Chunk 0 is always the metadata chunk (title + summary + first prompt).
// Subsequent chunks are content chunks built from user+assistant turn pairs.
func ChunkConversation(entry SessionEntry) ([]Chunk, error) {
	var chunks []Chunk

	// Chunk 0: metadata
	var metaParts []string
	if entry.CustomTitle != "" {
		metaParts = append(metaParts, "Title: "+entry.CustomTitle)
	}
	if entry.Summary != "" {
		metaParts = append(metaParts, "Summary: "+entry.Summary)
	}
	if entry.FirstPrompt != "" {
		metaParts = append(metaParts, "First prompt: "+entry.FirstPrompt)
	}
	if entry.ProjectPath != "" {
		metaParts = append(metaParts, "Project: "+entry.ProjectPath)
	}

	if len(metaParts) > 0 {
		metaText := strings.Join(metaParts, "\n")
		if len(metaText) > maxChunkChars {
			metaText = metaText[:maxChunkChars]
		}
		chunks = append(chunks, Chunk{Index: 0, Type: "metadata", Text: metaText})
	}

	// Read full conversation content for content chunks
	contentChunks, err := chunkConversationContent(entry.FullPath)
	if err != nil {
		// If we can't read content, return just metadata
		if len(chunks) > 0 {
			return chunks, nil
		}
		return nil, err
	}

	chunkIndex := 1
	for _, text := range contentChunks {
		chunks = append(chunks, Chunk{Index: chunkIndex, Type: "content", Text: text})
		chunkIndex++
	}

	return chunks, nil
}

// chunkConversationContent reads a .jsonl file and groups messages into chunks
// at turn boundaries (user + assistant pairs).
func chunkConversationContent(filePath string) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	// Collect turns: each turn is a user message followed by assistant response(s)
	type turn struct {
		role string
		text string
	}
	var turns []turn

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var msg jsonlMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		// Only care about user and assistant messages
		if msg.Type != "user" && msg.Type != "assistant" {
			continue
		}

		text := extractMessageContent(msg.Message.Content)
		if text == "" {
			continue
		}

		// Skip system-injected messages
		if msg.Type == "user" && isSystemInjectedMessage(text) {
			continue
		}
		if strings.HasPrefix(text, "[Request interrupted") {
			continue
		}

		role := "user"
		if msg.Type == "assistant" || msg.Message.Role == "assistant" {
			role = "assistant"
		}

		turns = append(turns, turn{role: role, text: text})
	}

	if len(turns) == 0 {
		return nil, nil
	}

	// Group turns into chunks at turn-pair boundaries
	var chunks []string
	var buffer strings.Builder

	for _, t := range turns {
		entry := fmt.Sprintf("[%s]: %s\n", t.role, t.text)

		// If adding this would exceed the limit and we have content, emit current buffer
		if buffer.Len()+len(entry) > maxChunkChars && buffer.Len() > 0 {
			chunks = append(chunks, buffer.String())
			buffer.Reset()
		}

		// If a single entry exceeds the limit, truncate it
		if len(entry) > maxChunkChars {
			entry = entry[:maxChunkChars]
		}

		buffer.WriteString(entry)
	}

	// Emit remainder
	if buffer.Len() > 0 {
		chunks = append(chunks, buffer.String())
	}

	return chunks, nil
}

// Float32ToBytes converts a float32 slice to raw bytes for SQLite storage.
func Float32ToBytes(f []float32) []byte {
	buf := make([]byte, len(f)*4)
	for i, v := range f {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

// BytesToFloat32 converts raw bytes back to a float32 slice.
func BytesToFloat32(b []byte) []float32 {
	f := make([]float32, len(b)/4)
	for i := range f {
		f[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return f
}

// CosineSimilarity computes the cosine similarity between two vectors.
func CosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float32
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / float32(math.Sqrt(float64(normA))*math.Sqrt(float64(normB)))
}

// IndexConversation chunks and embeds a single conversation, storing results in the DB.
// Embeds chunks individually so one oversized chunk doesn't fail the whole conversation.
func IndexConversation(entry SessionEntry, client *OllamaClient) (int, error) {
	chunks, err := ChunkConversation(entry)
	if err != nil {
		return 0, fmt.Errorf("chunk conversation %s: %w", entry.SessionID[:8], err)
	}

	if len(chunks) == 0 {
		return 0, nil
	}

	// Delete old embeddings for this conversation (in case chunk count changed)
	if err := db.DeleteEmbeddingsForConv(entry.SessionID); err != nil {
		return 0, fmt.Errorf("delete old embeddings: %w", err)
	}

	now := time.Now()
	stored := 0

	for _, chunk := range chunks {
		// EmbedOne handles context length errors internally by reducing size
		embedding, err := client.EmbedOne(chunk.Text)
		if err != nil {
			// Skip this chunk but continue with others
			continue
		}

		row := &db.EmbeddingRow{
			ConvID:     entry.SessionID,
			ChunkIndex: chunk.Index,
			ChunkType:  chunk.Type,
			ChunkText:  chunk.Text,
			Embedding:  Float32ToBytes(embedding),
			Model:      client.Model,
			CreatedAt:  now,
		}
		if err := db.UpsertEmbedding(row); err != nil {
			return stored, fmt.Errorf("store embedding chunk %d: %w", chunk.Index, err)
		}
		stored++
	}

	return stored, nil
}

// SearchResult holds a conversation match with its similarity score and matching chunk.
type EmbedSearchResult struct {
	Entry      SessionEntry
	Similarity float32
	ChunkText  string
	ChunkType  string
}

// SearchEmbeddings searches all stored embeddings for the query and returns top-K results.
func SearchEmbeddings(queryEmbedding []float32, entries []SessionEntry, topK int) ([]EmbedSearchResult, error) {
	// Load all embeddings
	allEmbeddings, err := db.ListAllEmbeddings()
	if err != nil {
		return nil, fmt.Errorf("load embeddings: %w", err)
	}

	if len(allEmbeddings) == 0 {
		return nil, nil
	}

	// Build entry lookup
	entryByID := make(map[string]SessionEntry, len(entries))
	for _, e := range entries {
		entryByID[e.SessionID] = e
	}

	// Find best-matching chunk per conversation
	type convMatch struct {
		convID     string
		similarity float32
		chunkText  string
		chunkType  string
	}
	bestByConv := make(map[string]*convMatch)

	for _, emb := range allEmbeddings {
		vec := BytesToFloat32(emb.Embedding)
		sim := CosineSimilarity(queryEmbedding, vec)

		if best, ok := bestByConv[emb.ConvID]; !ok || sim > best.similarity {
			bestByConv[emb.ConvID] = &convMatch{
				convID:     emb.ConvID,
				similarity: sim,
				chunkText:  emb.ChunkText,
				chunkType:  emb.ChunkType,
			}
		}
	}

	// Convert to results and sort by similarity
	var results []EmbedSearchResult
	for _, match := range bestByConv {
		entry, ok := entryByID[match.convID]
		if !ok {
			continue
		}
		results = append(results, EmbedSearchResult{
			Entry:      entry,
			Similarity: match.similarity,
			ChunkText:  match.chunkText,
			ChunkType:  match.chunkType,
		})
	}

	// Sort by similarity descending
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].Similarity > results[i].Similarity {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	if topK > 0 && topK < len(results) {
		results = results[:topK]
	}

	return results, nil
}
