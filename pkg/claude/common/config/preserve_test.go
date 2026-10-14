package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A Load → edit → Save round-trip must keep keys the schema does not model
// (custom, typo'd, or from a newer tclaude) at every object depth, while a
// retired key and a key inside an array element are dropped.
func TestSave_PreservesUnmodeledKeys(t *testing.T) {
	isolateConfigHome(t)
	require.NoError(t, os.MkdirAll(DataDir(), 0o700))
	require.NoError(t, os.WriteFile(ConfigPath(), []byte(`{
		"log_level": "info",
		"zzz_custom": {"nested": [1, 2.50, "x"]},
		"dashboard": {"show_debug_tab": true, "my_flag": "keep"},
		"agent": {
			"future_flag": true,
			"sudo": {"overrides": {
				"alice": {"max_duration": "1h", "bogus": 2},
				"bob": {"max_duration": "2h", "gone": 3}
			}}
		},
		"pre_compact_guard": {"thresholds": [{"window_size": 200000, "min_tokens": 1000, "lost": 1}]}
	}`), 0o644))

	cfg, err := Load()
	require.NoError(t, err)
	cfg.LogLevel = "debug"
	delete(cfg.Agent.Sudo.Overrides, "bob")
	require.NoError(t, Save(cfg))

	data, err := os.ReadFile(ConfigPath())
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got), "saved file must be valid JSON:\n%s", data)

	assert.Equal(t, "debug", got["log_level"], "the edit itself lands")
	assert.Equal(t, map[string]any{"nested": []any{1.0, 2.5, "x"}}, got["zzz_custom"], "top-level unknown kept")
	assert.Contains(t, string(data), `2.50`, "numbers are carried over verbatim")
	dash := got["dashboard"].(map[string]any)
	assert.Equal(t, "keep", dash["my_flag"], "unknown key in a modeled block kept")
	assert.NotContains(t, dash, "show_debug_tab", "a retired key is dropped")
	agent := got["agent"].(map[string]any)
	assert.Equal(t, true, agent["future_flag"])
	overrides := agent["sudo"].(map[string]any)["overrides"].(map[string]any)
	assert.Equal(t, 2.0, overrides["alice"].(map[string]any)["bogus"], "unknown key under a map entry kept")
	assert.NotContains(t, overrides, "bob", "a deleted map entry is not resurrected by its unknown keys")
	assert.NotContains(t, string(data), `"lost"`, "keys inside array elements are not carried over")

	// Modeled keys keep the typed encoding's order; carried keys follow.
	assert.Less(t, strings.Index(string(data), `"log_level"`), strings.Index(string(data), `"zzz_custom"`))

	// The kept keys survive another Load/Save cycle unchanged.
	again, err := Load()
	require.NoError(t, err)
	require.NoError(t, Save(again))
	data2, err := os.ReadFile(ConfigPath())
	require.NoError(t, err)
	assert.Equal(t, string(data), string(data2), "a no-op save is byte-stable")
}

// With nothing unmodeled on disk, Save writes exactly the typed encoding.
func TestSave_NoUnmodeledKeysWritesTypedEncoding(t *testing.T) {
	isolateConfigHome(t)
	cfg := DefaultConfig()
	require.NoError(t, Save(cfg))
	require.NoError(t, Save(cfg))
	data, err := os.ReadFile(ConfigPath())
	require.NoError(t, err)
	want, err := json.MarshalIndent(cfg, "", "  ")
	require.NoError(t, err)
	assert.Equal(t, string(want), string(data))
}

func TestUnknownConfigKeys(t *testing.T) {
	preserved, dropped := UnknownConfigKeys([]byte(`{
		"zzz": 1,
		"dashboard": {"show_debug_tab": true},
		"agent": {"future_flag": true, "sudo": {"overrides": {"alice": {"bogus": 2}}}},
		"pre_compact_guard": {"thresholds": [{"window_size": 1, "lost": 1}]}
	}`))
	assert.Equal(t, []string{"agent.future_flag", "agent.sudo.overrides.alice.bogus", "zzz"}, preserved)
	assert.Equal(t, []string{"pre_compact_guard.thresholds[0].lost"}, dropped)

	preserved, dropped = UnknownConfigKeys([]byte(`not json`))
	assert.Empty(t, preserved)
	assert.Empty(t, dropped)
}

// Clearing a block (nil pointer, often "off") must stick: its unmodeled keys
// must not recreate it, or the next load would see it switched back on.
func TestSave_ClearedBlockStaysCleared(t *testing.T) {
	isolateConfigHome(t)
	require.NoError(t, os.MkdirAll(DataDir(), 0o700))
	require.NoError(t, os.WriteFile(ConfigPath(), []byte(`{"ratelimit":{"five_hour_percent_max_used":50,"future_knob":1}}`), 0o644))

	cfg, err := Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.RateLimit)
	cfg.RateLimit = nil
	require.NoError(t, Save(cfg))

	data, err := os.ReadFile(ConfigPath())
	require.NoError(t, err)
	assert.NotContains(t, string(data), "ratelimit")
	reloaded, err := Load()
	require.NoError(t, err)
	assert.Nil(t, reloaded.RateLimit, "the cleared block stays cleared")
}

// encoding/json matches keys case-insensitively, so a case variant of a
// modeled key is that field, not an unknown key to carry: carrying it would
// make it win over the edited value on the next load.
func TestSave_CaseVariantKeyIsModeled(t *testing.T) {
	isolateConfigHome(t)
	require.NoError(t, os.MkdirAll(DataDir(), 0o700))
	require.NoError(t, os.WriteFile(ConfigPath(), []byte(`{"Log_Level":"debug","Agent":{"Future_Flag":true}}`), 0o644))

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "debug", cfg.LogLevel)
	cfg.LogLevel = "warn"
	require.NoError(t, Save(cfg))

	data, err := os.ReadFile(ConfigPath())
	require.NoError(t, err)
	assert.NotContains(t, string(data), "Log_Level")
	assert.Contains(t, string(data), "Future_Flag", "an unknown key under a case-variant block is still carried")
	reloaded, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "warn", reloaded.LogLevel, "the edit is not reverted by the old spelling")

	preserved, _ := UnknownConfigKeys([]byte(`{"Log_Level":"debug","Agent":{"Future_Flag":true}}`))
	assert.Equal(t, []string{"agent.Future_Flag"}, preserved)
}
