package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setTestHookCommand overrides HookCommand and rebuilds RequiredHooks for testing
func setTestHookCommand(t *testing.T) {
	t.Helper()
	origCmd := HookCommand
	origHooks := RequiredHooks

	HookCommand = "/test/bin/tclaude session hook-callback"
	hook := HookConfig{Type: "command", Command: HookCommand}
	newMatcher := func() HookMatcher { return HookMatcher{Hooks: []HookConfig{hook}} }
	RequiredHooks = map[string][]HookMatcher{
		"UserPromptSubmit":   {newMatcher()},
		"Stop":               {newMatcher()},
		"PermissionRequest":  {newMatcher()},
		"PreToolUse":         {newMatcher()},
		"PostToolUse":        {newMatcher()},
		"PostToolUseFailure": {newMatcher()},
		"SubagentStart":      {newMatcher()},
		"SubagentStop":       {newMatcher()},
		"Notification":       {{Hooks: []HookConfig{hook}}},
		"SessionStart":       {newMatcher()},
		"SessionEnd":         {newMatcher()},
		"PostCompact":        {newMatcher()},
	}

	t.Cleanup(func() {
		HookCommand = origCmd
		RequiredHooks = origHooks
	})
}

func TestIsOurHook(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{"/usr/local/bin/tclaude session hook-callback", true},
		{"/home/user/go/bin/tclaude session hook-callback", true},
		{"tclaude session hook-callback", true},
		{"some-other-tool do-thing", false},
		{"", false},
	}
	for _, tt := range tests {
		got := isOurHook(tt.command)
		assert.Equalf(t, tt.want, got, "isOurHook(%q)", tt.command)
	}
}

func TestNeedsHookCleanup(t *testing.T) {
	setTestHookCommand(t)
	currentCmd := HookCommand
	staleCmd := "/old/path/tclaude session hook-callback"
	otherCmd := "some-other-tool do-thing"

	tests := []struct {
		name     string
		matchers []HookMatcher
		want     bool
	}{
		{
			name:     "single current hook - no cleanup needed",
			matchers: []HookMatcher{{Hooks: []HookConfig{{Type: "command", Command: currentCmd}}}},
			want:     false,
		},
		{
			name: "duplicate current hooks - needs cleanup",
			matchers: []HookMatcher{
				{Hooks: []HookConfig{{Type: "command", Command: currentCmd}}},
				{Hooks: []HookConfig{{Type: "command", Command: currentCmd}}},
			},
			want: true,
		},
		{
			name:     "stale hook - needs cleanup",
			matchers: []HookMatcher{{Hooks: []HookConfig{{Type: "command", Command: staleCmd}}}},
			want:     true,
		},
		{
			name: "current + stale hooks - needs cleanup",
			matchers: []HookMatcher{
				{Hooks: []HookConfig{{Type: "command", Command: currentCmd}}},
				{Hooks: []HookConfig{{Type: "command", Command: staleCmd}}},
			},
			want: true,
		},
		{
			name:     "non-tclaude hook only - no cleanup needed",
			matchers: []HookMatcher{{Hooks: []HookConfig{{Type: "command", Command: otherCmd}}}},
			want:     false,
		},
		{
			name: "current hook + non-tclaude hook - no cleanup needed",
			matchers: []HookMatcher{
				{Hooks: []HookConfig{{Type: "command", Command: currentCmd}}},
				{Hooks: []HookConfig{{Type: "command", Command: otherCmd}}},
			},
			want: false,
		},
		{
			name:     "empty matchers - no cleanup needed",
			matchers: []HookMatcher{},
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, _ := json.Marshal(tt.matchers)
			got := needsHookCleanup(string(data))
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRemoveOurHooksFromEvent(t *testing.T) {
	setTestHookCommand(t)
	currentCmd := HookCommand
	otherCmd := "some-other-tool do-thing"
	staleCmd := "/old/path/tclaude session hook-callback"

	tests := []struct {
		name        string
		matchers    []HookMatcher
		wantRemoved bool
		wantNil     bool // true if all hooks removed (signal to delete event)
		wantCount   int  // number of remaining matchers (if not nil)
	}{
		{
			name: "removes all tclaude hooks",
			matchers: []HookMatcher{
				{Hooks: []HookConfig{{Type: "command", Command: currentCmd}}},
				{Hooks: []HookConfig{{Type: "command", Command: currentCmd}}},
			},
			wantRemoved: true,
			wantNil:     true,
		},
		{
			name: "removes stale hooks",
			matchers: []HookMatcher{
				{Hooks: []HookConfig{{Type: "command", Command: staleCmd}}},
			},
			wantRemoved: true,
			wantNil:     true,
		},
		{
			name: "keeps non-tclaude hooks",
			matchers: []HookMatcher{
				{Hooks: []HookConfig{{Type: "command", Command: currentCmd}}},
				{Hooks: []HookConfig{{Type: "command", Command: otherCmd}}},
			},
			wantRemoved: true,
			wantNil:     false,
			wantCount:   1,
		},
		{
			name: "no tclaude hooks - no change",
			matchers: []HookMatcher{
				{Hooks: []HookConfig{{Type: "command", Command: otherCmd}}},
			},
			wantRemoved: false,
			wantCount:   1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, _ := json.Marshal(tt.matchers)
			result, removed, err := removeOurHooksFromEvent(data)
			require.NoError(t, err)
			assert.Equal(t, tt.wantRemoved, removed, "removed")
			if tt.wantNil {
				assert.Nilf(t, result, "expected nil result, got %s", string(result))
			} else if result != nil {
				var remaining []HookMatcher
				require.NoError(t, json.Unmarshal(result, &remaining), "failed to unmarshal result")
				assert.Len(t, remaining, tt.wantCount, "remaining matchers")
			}
		})
	}
}

func TestInstallHooks_Idempotent(t *testing.T) {
	setTestHookCommand(t)

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	settingsPath := filepath.Join(tmpDir, ".claude", "settings.json")

	// Verify our override works
	require.Equal(t, settingsPath, ClaudeSettingsPath())

	countHooks := func() map[string]int {
		data, err := os.ReadFile(settingsPath)
		require.NoError(t, err, "read settings")
		var settings map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(data, &settings), "parse settings")
		var hooks map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(settings["hooks"], &hooks), "parse hooks")
		counts := make(map[string]int)
		for event, raw := range hooks {
			var matchers []HookMatcher
			require.NoErrorf(t, json.Unmarshal(raw, &matchers), "parse %s", event)
			for _, m := range matchers {
				for _, h := range m.Hooks {
					if isOurHook(h.Command) {
						counts[event]++
					}
				}
			}
		}
		return counts
	}

	// Install 3 times, verify exactly 1 hook per event each time
	for i := 1; i <= 3; i++ {
		require.NoErrorf(t, InstallHooks(), "InstallHooks (round %d)", i)
		counts := countHooks()
		for event := range RequiredHooks {
			assert.Equalf(t, 1, counts[event], "after install %d: %s has %d tclaude hooks, want 1", i, event, counts[event])
		}
	}
}

func TestInstallHooks_PreservesNonTclaudeHooks(t *testing.T) {
	setTestHookCommand(t)

	tmpDir := t.TempDir()
	settingsPath := filepath.Join(tmpDir, ".claude", "settings.json")
	t.Setenv("HOME", tmpDir)

	// Create settings with a non-tclaude hook on an event we also use
	require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0755))
	initialSettings := map[string]any{
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "my-custom-tool notify"},
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(initialSettings, "", "  ")
	require.NoError(t, os.WriteFile(settingsPath, data, 0644))

	require.NoError(t, InstallHooks(), "InstallHooks")

	// Read back and check
	result, _ := os.ReadFile(settingsPath)
	var settings map[string]json.RawMessage
	_ = json.Unmarshal(result, &settings)
	var hooks map[string]json.RawMessage
	_ = json.Unmarshal(settings["hooks"], &hooks)

	var stopMatchers []HookMatcher
	_ = json.Unmarshal(hooks["Stop"], &stopMatchers)

	customFound := false
	tclaudeCount := 0
	for _, m := range stopMatchers {
		for _, h := range m.Hooks {
			if h.Command == "my-custom-tool notify" {
				customFound = true
			}
			if isOurHook(h.Command) {
				tclaudeCount++
			}
		}
	}

	assert.True(t, customFound, "non-tclaude hook was removed")
	assert.Equal(t, 1, tclaudeCount, "Stop tclaude hooks")
}

func TestInstallHooks_CleansDuplicates(t *testing.T) {
	setTestHookCommand(t)

	tmpDir := t.TempDir()
	settingsPath := filepath.Join(tmpDir, ".claude", "settings.json")
	t.Setenv("HOME", tmpDir)

	// Create settings with duplicate tclaude hooks (simulating the bug)
	require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0755))
	hook := map[string]any{"type": "command", "command": HookCommand}
	matcher := map[string]any{"hooks": []any{hook}}
	duplicateHooks := map[string]any{
		"hooks": map[string]any{
			"Stop":         []any{matcher, matcher, matcher}, // triple duplicate
			"Notification": []any{matcher, matcher},          // double duplicate
		},
	}
	data, _ := json.MarshalIndent(duplicateHooks, "", "  ")
	require.NoError(t, os.WriteFile(settingsPath, data, 0644))

	require.NoError(t, InstallHooks(), "InstallHooks")

	// Verify all events now have exactly 1 hook
	result, _ := os.ReadFile(settingsPath)
	var settings map[string]json.RawMessage
	_ = json.Unmarshal(result, &settings)
	var hooks map[string]json.RawMessage
	_ = json.Unmarshal(settings["hooks"], &hooks)

	for event := range RequiredHooks {
		var matchers []HookMatcher
		_ = json.Unmarshal(hooks[event], &matchers)
		count := 0
		for _, m := range matchers {
			for _, h := range m.Hooks {
				if isOurHook(h.Command) {
					count++
				}
			}
		}
		assert.Equalf(t, 1, count, "%s has %d tclaude hooks after cleanup, want 1", event, count)
	}
}

func TestCheckHooksInstalled_DetectsDuplicates(t *testing.T) {
	setTestHookCommand(t)

	tmpDir := t.TempDir()
	settingsPath := filepath.Join(tmpDir, ".claude", "settings.json")
	t.Setenv("HOME", tmpDir)

	require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0755))

	// Install hooks normally first
	require.NoError(t, InstallHooks(), "InstallHooks")

	// Verify clean state
	installed, _, needsRepair := CheckHooksInstalled()
	assert.True(t, installed, "expected installed = true")
	assert.False(t, needsRepair, "expected needsRepair = false for clean install")

	// Manually inject a duplicate to simulate the bug
	data, _ := os.ReadFile(settingsPath)
	var settings map[string]json.RawMessage
	_ = json.Unmarshal(data, &settings)
	var hooks map[string]json.RawMessage
	_ = json.Unmarshal(settings["hooks"], &hooks)

	// Double up the Stop hook
	var stopMatchers []json.RawMessage
	_ = json.Unmarshal(hooks["Stop"], &stopMatchers)
	stopMatchers = append(stopMatchers, stopMatchers[0])
	hooks["Stop"], _ = json.Marshal(stopMatchers)
	settings["hooks"], _ = json.Marshal(hooks)
	output, _ := json.MarshalIndent(settings, "", "  ")
	require.NoError(t, os.WriteFile(settingsPath, output, 0644))

	// Check should detect the duplicate
	installed, _, needsRepair = CheckHooksInstalled()
	assert.True(t, installed, "expected installed = true (hooks are present, just duplicated)")
	assert.True(t, needsRepair, "expected needsRepair = true for duplicate hooks")
}
