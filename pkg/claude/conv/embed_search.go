package conv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

// --- index-embeddings command ---

type IndexEmbeddingsParams struct {
	Global  bool   `short:"g" help:"Index conversations from all projects"`
	Reindex bool   `long:"reindex" help:"Force re-index all conversations (ignore cache)"`
	Model   string `long:"model" env:"TCLAUDE_EMBED_MODEL" help:"Embedding model name" default:"qwen3-embedding:0.6b"`
	URL     string `long:"url" env:"TCLAUDE_OLLAMA_URL" help:"Ollama API base URL" default:"http://localhost:11434"`
}

func IndexEmbeddingsCmd() *cobra.Command {
	return boa.CmdT[IndexEmbeddingsParams]{
		Use:         "index-embeddings",
		Aliases:     []string{"idx-emb"},
		Short:       "Build semantic search index using local embeddings",
		Long:        "Chunks conversations and generates embeddings via Ollama for semantic search.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *IndexEmbeddingsParams, cmd *cobra.Command, args []string) {
			exitCode := RunIndexEmbeddings(params, os.Stdout, os.Stderr)
			if exitCode != 0 {
				os.Exit(exitCode)
			}
		},
	}.ToCobra()
}

func RunIndexEmbeddings(params *IndexEmbeddingsParams, stdout, stderr *os.File) int {
	client := NewOllamaClient(params.URL, params.Model)

	// Test connection
	_, err := client.EmbedOne("test")
	if err != nil {
		fmt.Fprintf(stderr, "Error connecting to Ollama: %v\n", err)
		fmt.Fprintf(stderr, "\nMake sure Ollama is running:\n")
		fmt.Fprintf(stderr, "  brew services start ollama\n")
		fmt.Fprintf(stderr, "  ollama pull %s\n", params.Model)
		return 1
	}

	// Load conversations
	var allEntries []SessionEntry

	if params.Global {
		projectsDir := ClaudeProjectsDir()
		entries, err := os.ReadDir(projectsDir)
		if err != nil {
			fmt.Fprintf(stderr, "Error reading projects directory: %v\n", err)
			return 1
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			projectPath := filepath.Join(projectsDir, entry.Name())
			index, err := LoadSessionsIndex(projectPath)
			if err != nil {
				continue
			}
			allEntries = append(allEntries, index.Entries...)
		}
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "Error getting current directory: %v\n", err)
			return 1
		}
		projectPath := GetClaudeProjectPath(cwd)
		if _, err := os.Stat(projectPath); os.IsNotExist(err) {
			fmt.Fprintf(stderr, "No Claude conversations found for %s\n", cwd)
			return 1
		}
		index, err := LoadSessionsIndex(projectPath)
		if err != nil {
			fmt.Fprintf(stderr, "Error loading sessions index: %v\n", err)
			return 1
		}
		allEntries = index.Entries
	}

	if len(allEntries) == 0 {
		fmt.Fprintf(stdout, "No conversations found\n")
		return 0
	}

	// Build set of valid conversation IDs
	validIDs := make(map[string]bool, len(allEntries))
	for _, e := range allEntries {
		validIDs[e.SessionID] = true
	}

	// Clean up orphaned embeddings (conversations deleted from disk)
	embeddedConvs, err := db.ListEmbeddedConvIDs()
	if err != nil {
		fmt.Fprintf(stderr, "Error checking existing embeddings: %v\n", err)
		return 1
	}
	orphans := 0
	for convID := range embeddedConvs {
		if !validIDs[convID] {
			if err := db.DeleteEmbeddingsForConv(convID); err == nil {
				orphans++
			}
		}
	}
	if orphans > 0 {
		fmt.Fprintf(stdout, "Cleaned up %d orphaned embedding(s)\n", orphans)
	}

	// For --reindex, wipe everything and start fresh
	if params.Reindex {
		if err := db.DeleteAllEmbeddings(); err != nil {
			fmt.Fprintf(stderr, "Error clearing embeddings: %v\n", err)
			return 1
		}
		// Refresh after wipe
		embeddedConvs = make(map[string]time.Time)
	}

	// Filter to only conversations that need indexing
	var toIndex []SessionEntry
	for _, entry := range allEntries {
		embeddedAt, exists := embeddedConvs[entry.SessionID]
		if !exists {
			toIndex = append(toIndex, entry)
			continue
		}

		// Re-index if the conversation file has been modified since last embedding
		if entry.FileMtime > embeddedAt.Unix() {
			toIndex = append(toIndex, entry)
		}
	}

	if len(toIndex) == 0 {
		fmt.Fprintf(stdout, "All %d conversations are already indexed\n", len(allEntries))
		return 0
	}

	fmt.Fprintf(stdout, "Indexing %d conversations (%d already indexed)...\n",
		len(toIndex), len(allEntries)-len(toIndex))

	start := time.Now()
	totalChunks := 0
	errors := 0

	for i, entry := range toIndex {
		title := entry.DisplayTitle()
		if len(title) > 60 {
			title = title[:57] + "..."
		}
		fmt.Fprintf(stdout, "  [%d/%d] %s %s", i+1, len(toIndex), entry.SessionID[:8], title)

		convStart := time.Now()
		chunks, err := IndexConversation(entry, client)
		if err != nil {
			fmt.Fprintf(stdout, " - ERROR (%v)\n", time.Since(convStart).Round(time.Millisecond))
			fmt.Fprintf(stderr, "    %v\n", err)
			errors++
			continue
		}
		totalChunks += chunks
		fmt.Fprintf(stdout, " - %d chunks (%v)\n", chunks, time.Since(convStart).Round(time.Millisecond))
	}

	elapsed := time.Since(start)
	fmt.Fprintf(stdout, "\nDone in %v: %d conversations, %d chunks, %d errors\n",
		elapsed.Round(time.Millisecond), len(toIndex)-errors, totalChunks, errors)

	return 0
}

// --- search-embeddings command ---

type SearchEmbeddingsParams struct {
	Query  string `pos:"true" help:"Natural language search query"`
	Global bool   `short:"g" help:"Search across all projects"`
	Limit  int    `short:"n" help:"Number of results" default:"10"`
	Long   bool   `short:"l" help:"Show matching chunk text"`
	JSON   bool   `long:"json" help:"Output as JSON"`
	Model  string `long:"model" env:"TCLAUDE_EMBED_MODEL" help:"Embedding model name" default:"qwen3-embedding:0.6b"`
	URL    string `long:"url" env:"TCLAUDE_OLLAMA_URL" help:"Ollama API base URL" default:"http://localhost:11434"`
}

func SearchEmbeddingsCmd() *cobra.Command {
	return boa.CmdT[SearchEmbeddingsParams]{
		Use:         "search-embeddings",
		Aliases:     []string{"sem", "semantic", "ai", "ask", "ai-search"},
		Short:       "Semantic search across conversations",
		Long:        "Search conversations by meaning using local embeddings (requires prior index-embeddings).",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *SearchEmbeddingsParams, cmd *cobra.Command, args []string) {
			exitCode := RunSearchEmbeddings(params, os.Stdout, os.Stderr)
			if exitCode != 0 {
				os.Exit(exitCode)
			}
		},
	}.ToCobra()
}

func RunSearchEmbeddings(params *SearchEmbeddingsParams, stdout, stderr *os.File) int {
	if params.Query == "" {
		fmt.Fprintf(stderr, "Query is required\n")
		return 1
	}

	client := NewOllamaClient(params.URL, params.Model)

	// Check for model mismatch with existing index
	if models, err := db.ListEmbeddingModels(); err == nil && len(models) > 0 {
		mismatch := false
		for _, m := range models {
			if m != params.Model {
				mismatch = true
				break
			}
		}
		if mismatch {
			fmt.Fprintf(stderr, "Error: index was built with model %q, but searching with %q\n", models[0], params.Model)
			fmt.Fprintf(stderr, "Run 'tclaude conv index-embeddings --reindex' to rebuild with the new model\n")
			return 1
		}
	}

	// Embed the query
	queryEmbedding, err := client.EmbedOne(params.Query)
	if err != nil {
		fmt.Fprintf(stderr, "Error embedding query: %v\n", err)
		fmt.Fprintf(stderr, "\nMake sure Ollama is running:\n")
		fmt.Fprintf(stderr, "  brew services start ollama\n")
		return 1
	}

	// Load conversation entries for display
	var allEntries []SessionEntry

	if params.Global {
		projectsDir := ClaudeProjectsDir()
		entries, err := os.ReadDir(projectsDir)
		if err != nil {
			fmt.Fprintf(stderr, "Error reading projects directory: %v\n", err)
			return 1
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			projectPath := filepath.Join(projectsDir, entry.Name())
			index, err := LoadSessionsIndex(projectPath)
			if err != nil {
				continue
			}
			allEntries = append(allEntries, index.Entries...)
		}
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "Error getting current directory: %v\n", err)
			return 1
		}
		projectPath := GetClaudeProjectPath(cwd)
		if _, err := os.Stat(projectPath); os.IsNotExist(err) {
			fmt.Fprintf(stderr, "No Claude conversations found for %s\n", cwd)
			return 1
		}
		index, err := LoadSessionsIndex(projectPath)
		if err != nil {
			fmt.Fprintf(stderr, "Error loading sessions index: %v\n", err)
			return 1
		}
		allEntries = index.Entries
	}

	// Search
	results, err := SearchEmbeddings(queryEmbedding, allEntries, params.Limit)
	if err != nil {
		fmt.Fprintf(stderr, "Error searching: %v\n", err)
		return 1
	}

	if len(results) == 0 {
		fmt.Fprintf(stdout, "No results found. Have you run 'tclaude conv index-embeddings'?\n")
		return 0
	}

	// JSON output
	if params.JSON {
		type JSONResult struct {
			SessionEntry
			Similarity float32 `json:"similarity"`
			ChunkType  string  `json:"chunkType"`
			ChunkText  string  `json:"chunkText,omitempty"`
		}
		jsonResults := make([]JSONResult, len(results))
		for i, r := range results {
			jsonResults[i] = JSONResult{
				SessionEntry: r.Entry,
				Similarity:   r.Similarity,
				ChunkType:    r.ChunkType,
				ChunkText:    r.ChunkText,
			}
		}
		data, err := json.MarshalIndent(jsonResults, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "Error marshaling JSON: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, string(data))
		return 0
	}

	// Table output
	fmt.Fprintf(stdout, "\nSemantic search: %q\n\n", params.Query)

	entries := make([]SessionEntry, len(results))
	for i, r := range results {
		entries[i] = r.Entry
	}
	RenderTable(stdout, entries, params.Global, params.Long, nil)

	// Show similarity scores and matching chunks
	fmt.Fprintln(stdout)
	for _, r := range results {
		title := r.Entry.DisplayTitle()
		if len(title) > 60 {
			title = title[:57] + "..."
		}
		fmt.Fprintf(stdout, "  %.4f  %s  %s\n", r.Similarity, r.Entry.SessionID[:8], title)
		if params.Long {
			chunkPreview := r.ChunkText
			if len(chunkPreview) > 200 {
				chunkPreview = chunkPreview[:197] + "..."
			}
			chunkPreview = strings.ReplaceAll(chunkPreview, "\n", " ")
			fmt.Fprintf(stdout, "           [%s] %s\n", r.ChunkType, chunkPreview)
		}
	}

	fmt.Fprintf(stdout, "\n%d result(s)\n", len(results))
	return 0
}
