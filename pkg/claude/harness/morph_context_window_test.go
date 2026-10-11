package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMorphCodexWindowUsesExactFreshModelAndConfigCeiling(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	now := time.Now()
	cache := map[string]any{"fetched_at": now, "models": []map[string]any{{"slug": "small", "context_window": 16000, "effective_context_window_percent": 95}, {"slug": "large", "context_window": 1000000}}}
	raw, err := json.Marshal(cache)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "models_cache.json"), raw, 0600))
	h := MustGet(CodexName)
	n, source := MorphContextWindow(h, "small")
	assert.Equal(t, int64(15200), n)
	assert.Contains(t, source, "cached")
	n, source = MorphContextWindow(h, "unknown")
	assert.Equal(t, int64(32000), n)
	assert.Equal(t, "unknown model, assumed 32k", source)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("model = 'small'\nmodel_context_window = 12000\n"), 0600))
	n, source = MorphContextWindow(h, "")
	assert.Equal(t, int64(12000), n)
	assert.Contains(t, source, "configured")
	n, _ = MorphContextWindow(h, "large")
	assert.Equal(t, int64(12000), n, "native config's global cap wins")
	cache["fetched_at"] = now.Add(-25 * time.Hour)
	raw, err = json.Marshal(cache)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "models_cache.json"), raw, 0600))
	assert.Zero(t, codexCachedMorphWindow(filepath.Join(dir, "models_cache.json"), "small", now))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte("not JSON"), 0600))
	assert.Zero(t, codexCachedMorphWindow(filepath.Join(dir, "models_cache.json"), "small", now))
}

func TestMorphKnownWindowsAndUnknownAliases(t *testing.T) {
	cases := []struct {
		h, model string
		want     int64
	}{
		{DefaultName, "sonnet", 200000}, {DefaultName, "opus[1m]", 1000000},
		{GeminiName, "gemini-2.5-pro", 1048576}, {GeminiName, "gemma-4-31b-it", 256000},
		{GeminiName, "custom-small", 32000}, {GeminiName, "auto", 32000},
	}
	for _, c := range cases {
		t.Run(c.h+"/"+c.model, func(t *testing.T) { n, _ := MorphContextWindow(MustGet(c.h), c.model); assert.Equal(t, c.want, n) })
	}
}

func TestMorphOpenCodeWindowsUseProviderSpecificInputLimit(t *testing.T) {
	windows := parseOpenCodeModelWindows("local/tiny\n{\"limit\":{\"context\":8192,\"input\":6144}}\ncloud/tiny\n{\"limit\":{\"context\":200000}}\ncustom/unknown\n{\"limit\":{}}\n")
	assert.Equal(t, int64(6144), windows["local/tiny"])
	assert.Equal(t, int64(200000), windows["cloud/tiny"])
	_, ok := windows["custom/unknown"]
	assert.False(t, ok)
	openCodeModelCache.Lock()
	before := openCodeModelCache.value
	openCodeModelCache.value = openCodeCatalog{at: time.Now(), windows: windows}
	openCodeModelCache.Unlock()
	t.Cleanup(func() { openCodeModelCache.Lock(); openCodeModelCache.value = before; openCodeModelCache.Unlock() })
	n, source := MorphContextWindow(MustGet(OpenCodeName), "local/tiny")
	assert.Equal(t, int64(6144), n)
	assert.Contains(t, source, "cached")
}
