package notify

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common"
)

func TestBuildDarwinNotifyCmd(t *testing.T) {
	tests := []struct {
		name      string
		title     string
		body      string
		sessionID string
		clPath    string
		tmuxDir   string
		wantArgs  []string
	}{
		{
			name:      "with tmux dir",
			title:     "Claude: Idle",
			body:      "abc123 | myproject - Working on feature",
			sessionID: "abc123",
			clPath:    "/usr/local/bin/tclaude",
			tmuxDir:   "/opt/homebrew/bin",
			wantArgs: []string{
				"-title", "Claude: Idle",
				"-message", "abc123 | myproject - Working on feature",
				"-execute", "PATH=/opt/homebrew/bin:$PATH /usr/local/bin/tclaude session focus abc123",
				"-sound", "default",
			},
		},
		{
			name:      "without tmux dir",
			title:     "Claude: Awaiting permission",
			body:      "def456 | otherproject",
			sessionID: "def456",
			clPath:    "/home/user/go/bin/tclaude",
			tmuxDir:   "",
			wantArgs: []string{
				"-title", "Claude: Awaiting permission",
				"-message", "def456 | otherproject",
				"-execute", "/home/user/go/bin/tclaude session focus def456",
				"-sound", "default",
			},
		},
		{
			name:      "fallback path",
			title:     "Claude: Idle",
			body:      "test | proj",
			sessionID: "test",
			clPath:    "",
			tmuxDir:   "",
			wantArgs: []string{
				"-title", "Claude: Idle",
				"-message", "test | proj",
				"-execute", common.DetectAbsoluteCmd() + " session focus test",
				"-sound", "default",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := BuildDarwinNotifyCmd(tt.title, tt.body, tt.sessionID, tt.clPath, tt.tmuxDir)

			assert.Equal(t, "terminal-notifier", cmd.Program, "Program")

			require.Len(t, cmd.Args, len(tt.wantArgs), "Args length\nGot:  %v\nWant: %v", cmd.Args, tt.wantArgs)

			for i, arg := range cmd.Args {
				assert.Equal(t, tt.wantArgs[i], arg, "Args[%d]", i)
			}
		})
	}
}

func TestBuildDarwinFallbackCmd(t *testing.T) {
	cmd := BuildDarwinFallbackCmd("Claude: Idle", "test | proj")

	assert.Equal(t, "osascript", cmd.Program, "Program")

	require.Len(t, cmd.Args, 2, "Args length")

	assert.Equal(t, "-e", cmd.Args[0], "Args[0]")

	assert.Contains(t, cmd.Args[1], "display notification", "Args[1] should contain 'display notification'")
}

func TestBuildWSLNotifyCmd(t *testing.T) {
	cmd := BuildWSLNotifyCmd(
		"Claude: Idle",
		"abc123 | myproject",
		"/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe",
		"tclaude://focus/abc123",
	)

	assert.Equal(t, "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe", cmd.Program, "Program")

	require.GreaterOrEqual(t, len(cmd.Args), 4, "Args length")

	assert.Equal(t, "-NoProfile", cmd.Args[0], "Args[0]")
	assert.Equal(t, "-NonInteractive", cmd.Args[1], "Args[1]")
	assert.Equal(t, "-Command", cmd.Args[2], "Args[2]")

	// Check script contains expected elements
	script := cmd.Args[3]
	expectedParts := []string{
		"tclaude://focus/abc123",
		"Claude: Idle",
		"abc123 | myproject",
		"ToastNotification",
	}

	for _, part := range expectedParts {
		assert.Contains(t, script, part, "Script should contain %q", part)
	}
}

func TestNotificationBody(t *testing.T) {
	tests := []struct {
		name        string
		sessionID   string
		projectName string
		convTitle   string
		want        string
	}{
		{
			name:        "with conv title",
			sessionID:   "abc12345678",
			projectName: "myproject",
			convTitle:   "Working on feature",
			want:        "abc12345 | myproject - Working on feature",
		},
		{
			name:        "without conv title",
			sessionID:   "def456",
			projectName: "otherproj",
			convTitle:   "",
			want:        "def456 | otherproj",
		},
		{
			name:        "short session ID",
			sessionID:   "abc",
			projectName: "proj",
			convTitle:   "Title",
			want:        "abc | proj - Title",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NotificationBody(tt.sessionID, tt.projectName, tt.convTitle)
			assert.Equal(t, tt.want, got, "NotificationBody()")
		})
	}
}

func TestNotificationTitle(t *testing.T) {
	tests := []struct {
		status string
		want   string
	}{
		{"idle", "Claude: Idle"},
		{"working", "Claude: Working"},
		{"awaiting_permission", "Claude: Awaiting permission"},
		{"awaiting_input", "Claude: Awaiting input"},
		{"exited", "Claude: Exited"},
		{"unknown", "Claude: unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			got := NotificationTitle(tt.status)
			assert.Equal(t, tt.want, got, "NotificationTitle(%q)", tt.status)
		})
	}
}

func TestBuildDarwiniTermFocusCmd(t *testing.T) {
	cmd := BuildDarwiniTermFocusCmd("/dev/ttys003")

	assert.Equal(t, "osascript", cmd.Program, "Program")

	require.Len(t, cmd.Args, 2, "Args length")

	script := cmd.Args[1]
	expectedParts := []string{
		"iTerm2",
		"/dev/ttys003",
		"tty of s",
		"select t",
		"select w",
	}

	for _, part := range expectedParts {
		assert.Contains(t, script, part, "Script should contain %q", part)
	}
}

func TestFocusCommandString(t *testing.T) {
	tests := []struct {
		name      string
		clPath    string
		tmuxDir   string
		sessionID string
		wantFunc  func(tmuxDir string) string // dynamic expected value
	}{
		{
			name:      "with tmux dir",
			clPath:    "/usr/bin/tclaude",
			tmuxDir:   "/opt/homebrew/bin",
			sessionID: "abc123",
			wantFunc: func(tmuxDir string) string {
				return fmt.Sprintf("PATH=%s:$PATH /usr/bin/tclaude session focus abc123", filepath.Clean(tmuxDir))
			},
		},
		{
			name:      "without tmux dir",
			clPath:    "/usr/bin/tclaude",
			tmuxDir:   "",
			sessionID: "abc123",
			wantFunc: func(_ string) string {
				return "/usr/bin/tclaude session focus abc123"
			},
		},
		{
			name:      "fallback",
			clPath:    "",
			tmuxDir:   "",
			sessionID: "xyz",
			wantFunc: func(_ string) string {
				return common.DetectAbsoluteCmd() + " session focus xyz"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FocusCommandString(tt.clPath, tt.tmuxDir, tt.sessionID)
			want := tt.wantFunc(tt.tmuxDir)
			assert.Equal(t, want, got, "FocusCommandString()")
		})
	}
}

