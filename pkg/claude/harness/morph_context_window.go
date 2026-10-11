package harness

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
)

// MorphContextWindow is a best available input limit, not a request to enlarge
// the model's native window. No network or subprocess is started by this read.
func MorphContextWindow(h *Harness, model string) (int64, string) {
	model = strings.TrimSpace(model)
	switch h.Name {
	case DefaultName:
		if strings.HasSuffix(model, "[1m]") {
			return 1_000_000, "Claude extended-context selection"
		}
		if slices.Contains(h.Models.Models(), model) && model != "" {
			return 200_000, "Claude standard context"
		}
	case GeminiName:
		if slices.Contains(geminiKnownModels, model) && model != "auto" && model != "pro" && model != "flash" && model != "flash-lite" {
			return GeminiContextWindow(model), "Gemini CLI model limits"
		}
	case CodexName:
		if dir, err := CodexConfigDir(); err == nil {
			configuredModel, cap, publicRoute := codexConfiguredMorphWindow(filepath.Join(dir, "config.toml"))
			if model == "" {
				model = configuredModel
			}
			var n int64
			if publicRoute {
				n = codexCachedMorphWindow(filepath.Join(dir, "models_cache.json"), model, time.Now())
			}
			if cap > 0 && (n <= 0 || cap < n) {
				return cap, "Codex configured context window"
			}
			if n > 0 {
				return n, "Codex cached model metadata"
			}
		}
	case CopilotName:
		if n, found, err := db.FreshCopilotModelPromptLimit(model, time.Now(), 24*time.Hour); err == nil && found {
			return n, "Copilot model prompt limit"
		}
	case OpenCodeName:
		openCodeModelCache.Lock()
		catalog := openCodeModelCache.value
		openCodeModelCache.Unlock()
		if catalog.err == nil && !catalog.at.IsZero() && time.Since(catalog.at) < openCodeModelCacheTTL {
			if n := catalog.windows[model]; n > 0 {
				return n, "OpenCode cached model metadata"
			}
		}
	}
	return transcript.DefaultContextWindowTokens, "unknown model, assumed 32k"
}

// Codex stores the catalog locally. Read only the exact model's public limits;
// ignore account identity, prompts and other metadata. A stale or malformed
// catalog must not turn into a claim that a custom model has a large window.
func codexCachedMorphWindow(path, model string, now time.Time) int64 {
	if model == "" {
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	var cache struct {
		FetchedAt time.Time `json:"fetched_at"`
		Models    []struct {
			Slug    string `json:"slug"`
			Window  int64  `json:"context_window"`
			Percent int64  `json:"effective_context_window_percent"`
		} `json:"models"`
	}
	dec := json.NewDecoder(io.LimitReader(f, 8<<20))
	if dec.Decode(&cache) != nil || cache.FetchedAt.IsZero() || now.Sub(cache.FetchedAt) < 0 || now.Sub(cache.FetchedAt) >= 24*time.Hour {
		return 0
	}
	for _, m := range cache.Models {
		if m.Slug == model && m.Window > 0 && m.Window <= 10_000_000 {
			if m.Percent < 0 || m.Percent > 100 {
				return 0
			}
			if m.Percent > 0 && m.Percent <= 100 {
				return m.Window * m.Percent / 100
			}
			return m.Window
		}
	}
	return 0
}

// Verbose OpenCode output alternates provider/model lines and JSON objects.
// Decode the limit object instead of guessing from the provider's model name:
// custom endpoints can give familiar names much smaller context windows.
func parseOpenCodeModelWindows(output string) map[string]int64 {
	windows := map[string]int64{}
	reader := bufio.NewReader(strings.NewReader(output))
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return windows
		}
		id := strings.TrimSpace(line)
		if !strings.Contains(id, "/") || strings.ContainsAny(id, " \t{}\"") {
			continue
		}
		var metadata struct {
			Limit struct {
				Context int64 `json:"context"`
				Input   int64 `json:"input"`
			} `json:"limit"`
		}
		dec := json.NewDecoder(reader)
		err = dec.Decode(&metadata)
		reader = bufio.NewReader(io.MultiReader(dec.Buffered(), reader))
		if err != nil {
			continue
		}
		n := metadata.Limit.Context
		if metadata.Limit.Input > 0 && (n <= 0 || metadata.Limit.Input < n) {
			n = metadata.Limit.Input
		}
		if n > 0 && n <= 10_000_000 {
			windows[id] = n
		}
	}
}

func codexConfiguredMorphWindow(path string) (string, int64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, os.IsNotExist(err)
	}
	defer f.Close()
	var config struct {
		Provider  string `toml:"model_provider"`
		Providers map[string]struct {
			BaseURL string `toml:"base_url"`
		} `toml:"model_providers"`
		Model    string `toml:"model"`
		Window   int64  `toml:"model_context_window"`
		Profile  string `toml:"profile"`
		Profiles map[string]struct {
			Provider string `toml:"model_provider"`
			Model    string `toml:"model"`
			Window   int64  `toml:"model_context_window"`
		} `toml:"profiles"`
	}
	if toml.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&config) != nil {
		return "", 0, false
	}
	if profile, ok := config.Profiles[config.Profile]; ok {
		if profile.Provider != "" {
			config.Provider = profile.Provider
		}
		if profile.Model != "" {
			config.Model = profile.Model
		}
		if profile.Window != 0 {
			config.Window = profile.Window
		}
	}
	if config.Window < 0 || config.Window > 10_000_000 {
		config.Window = 0
	}
	publicRoute := (config.Provider == "" || config.Provider == "openai") && config.Providers["openai"].BaseURL == ""
	return config.Model, config.Window, publicRoute
}
