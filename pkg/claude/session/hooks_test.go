package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
		if got := isOurHook(tt.command); got != tt.want {
			t.Errorf("isOurHook(%q) = %v, want %v", tt.command, got, tt.want)
		}
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
			if got := needsHookCleanup(string(data)); got != tt.want {
				t.Errorf("needsHookCleanup() = %v, want %v", got, tt.want)
			}
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
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if removed != tt.wantRemoved {
				t.Errorf("removed = %v, want %v", removed, tt.wantRemoved)
			}
			if tt.wantNil {
				if result != nil {
					t.Errorf("expected nil result, got %s", string(result))
				}
			} else if result != nil {
				var remaining []HookMatcher
				if err := json.Unmarshal(result, &remaining); err != nil {
					t.Fatalf("failed to unmarshal result: %v", err)
				}
				if len(remaining) != tt.wantCount {
					t.Errorf("remaining matchers = %d, want %d", len(remaining), tt.wantCount)
				}
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
	if got := ClaudeSettingsPath(); got != settingsPath {
		t.Fatalf("ClaudeSettingsPath() = %q, want %q", got, settingsPath)
	}

	countHooks := func() map[string]int {
		data, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatalf("read settings: %v", err)
		}
		var settings map[string]json.RawMessage
		if err := json.Unmarshal(data, &settings); err != nil {
			t.Fatalf("parse settings: %v", err)
		}
		var hooks map[string]json.RawMessage
		if err := json.Unmarshal(settings["hooks"], &hooks); err != nil {
			t.Fatalf("parse hooks: %v", err)
		}
		counts := make(map[string]int)
		for event, raw := range hooks {
			var matchers []HookMatcher
			if err := json.Unmarshal(raw, &matchers); err != nil {
				t.Fatalf("parse %s: %v", event, err)
			}
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
		if err := InstallHooks(); err != nil {
			t.Fatalf("InstallHooks (round %d): %v", i, err)
		}
		counts := countHooks()
		for event := range RequiredHooks {
			if counts[event] != 1 {
				t.Errorf("after install %d: %s has %d tclaude hooks, want 1", i, event, counts[event])
			}
		}
	}
}

func TestInstallHooks_PreservesNonTclaudeHooks(t *testing.T) {
	setTestHookCommand(t)

	tmpDir := t.TempDir()
	settingsPath := filepath.Join(tmpDir, ".claude", "settings.json")
	t.Setenv("HOME", tmpDir)

	// Create settings with a non-tclaude hook on an event we also use
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		t.Fatal(err)
	}
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
	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	if err := InstallHooks(); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	// Read back and check
	result, _ := os.ReadFile(settingsPath)
	var settings map[string]json.RawMessage
	json.Unmarshal(result, &settings)
	var hooks map[string]json.RawMessage
	json.Unmarshal(settings["hooks"], &hooks)

	var stopMatchers []HookMatcher
	json.Unmarshal(hooks["Stop"], &stopMatchers)

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

	if !customFound {
		t.Error("non-tclaude hook was removed")
	}
	if tclaudeCount != 1 {
		t.Errorf("Stop has %d tclaude hooks, want 1", tclaudeCount)
	}
}

func TestInstallHooks_CleansDuplicates(t *testing.T) {
	setTestHookCommand(t)

	tmpDir := t.TempDir()
	settingsPath := filepath.Join(tmpDir, ".claude", "settings.json")
	t.Setenv("HOME", tmpDir)

	// Create settings with duplicate tclaude hooks (simulating the bug)
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		t.Fatal(err)
	}
	hook := map[string]any{"type": "command", "command": HookCommand}
	matcher := map[string]any{"hooks": []any{hook}}
	duplicateHooks := map[string]any{
		"hooks": map[string]any{
			"Stop":         []any{matcher, matcher, matcher}, // triple duplicate
			"Notification": []any{matcher, matcher},          // double duplicate
		},
	}
	data, _ := json.MarshalIndent(duplicateHooks, "", "  ")
	if err := os.WriteFile(settingsPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	if err := InstallHooks(); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	// Verify all events now have exactly 1 hook
	result, _ := os.ReadFile(settingsPath)
	var settings map[string]json.RawMessage
	json.Unmarshal(result, &settings)
	var hooks map[string]json.RawMessage
	json.Unmarshal(settings["hooks"], &hooks)

	for event := range RequiredHooks {
		var matchers []HookMatcher
		json.Unmarshal(hooks[event], &matchers)
		count := 0
		for _, m := range matchers {
			for _, h := range m.Hooks {
				if isOurHook(h.Command) {
					count++
				}
			}
		}
		if count != 1 {
			t.Errorf("%s has %d tclaude hooks after cleanup, want 1", event, count)
		}
	}
}

func TestCheckHooksInstalled_DetectsDuplicates(t *testing.T) {
	setTestHookCommand(t)

	tmpDir := t.TempDir()
	settingsPath := filepath.Join(tmpDir, ".claude", "settings.json")
	t.Setenv("HOME", tmpDir)

	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		t.Fatal(err)
	}

	// Install hooks normally first
	if err := InstallHooks(); err != nil {
		t.Fatalf("InstallHooks: %v", err)
	}

	// Verify clean state
	installed, _, needsRepair := CheckHooksInstalled()
	if !installed {
		t.Error("expected installed = true")
	}
	if needsRepair {
		t.Error("expected needsRepair = false for clean install")
	}

	// Manually inject a duplicate to simulate the bug
	data, _ := os.ReadFile(settingsPath)
	var settings map[string]json.RawMessage
	json.Unmarshal(data, &settings)
	var hooks map[string]json.RawMessage
	json.Unmarshal(settings["hooks"], &hooks)

	// Double up the Stop hook
	var stopMatchers []json.RawMessage
	json.Unmarshal(hooks["Stop"], &stopMatchers)
	stopMatchers = append(stopMatchers, stopMatchers[0])
	hooks["Stop"], _ = json.Marshal(stopMatchers)
	settings["hooks"], _ = json.Marshal(hooks)
	output, _ := json.MarshalIndent(settings, "", "  ")
	os.WriteFile(settingsPath, output, 0644)

	// Check should detect the duplicate
	installed, _, needsRepair = CheckHooksInstalled()
	if !installed {
		t.Error("expected installed = true (hooks are present, just duplicated)")
	}
	if !needsRepair {
		t.Error("expected needsRepair = true for duplicate hooks")
	}
}
