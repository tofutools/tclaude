package notify

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common"
)

func TestBuildDarwinNotifyCmd(t *testing.T) {
	tests := []struct {
		name      string
		title     string
		body      string
		sessionID string
		clPath  string
		tmuxDir   string
		wantArgs  []string
	}{
		{
			name:      "with tmux dir",
			title:     "Claude: Idle",
			body:      "abc123 | myproject - Working on feature",
			sessionID: "abc123",
			clPath:  "/usr/local/bin/tclaude",
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
			clPath:  "/home/user/go/bin/tclaude",
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
			clPath:  "",
			tmuxDir:   "",
			wantArgs: []string{
				"-title", "Claude: Idle",
				"-message", "test | proj",
				"-execute", common.DetectCmd() + " session focus test",
				"-sound", "default",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := BuildDarwinNotifyCmd(tt.title, tt.body, tt.sessionID, tt.clPath, tt.tmuxDir)

			if cmd.Program != "terminal-notifier" {
				t.Errorf("Program = %q, want %q", cmd.Program, "terminal-notifier")
			}

			if len(cmd.Args) != len(tt.wantArgs) {
				t.Errorf("Args length = %d, want %d\nGot:  %v\nWant: %v", len(cmd.Args), len(tt.wantArgs), cmd.Args, tt.wantArgs)
				return
			}

			for i, arg := range cmd.Args {
				if arg != tt.wantArgs[i] {
					t.Errorf("Args[%d] = %q, want %q", i, arg, tt.wantArgs[i])
				}
			}
		})
	}
}

func TestBuildDarwinFallbackCmd(t *testing.T) {
	cmd := BuildDarwinFallbackCmd("Claude: Idle", "test | proj")

	if cmd.Program != "osascript" {
		t.Errorf("Program = %q, want %q", cmd.Program, "osascript")
	}

	if len(cmd.Args) != 2 {
		t.Errorf("Args length = %d, want 2", len(cmd.Args))
		return
	}

	if cmd.Args[0] != "-e" {
		t.Errorf("Args[0] = %q, want %q", cmd.Args[0], "-e")
	}

	if !strings.Contains(cmd.Args[1], "display notification") {
		t.Errorf("Args[1] should contain 'display notification', got %q", cmd.Args[1])
	}
}

func TestBuildWSLNotifyCmd(t *testing.T) {
	cmd := BuildWSLNotifyCmd(
		"Claude: Idle",
		"abc123 | myproject",
		"/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe",
		"tclaude://focus/abc123",
	)

	if cmd.Program != "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe" {
		t.Errorf("Program = %q, want powershell path", cmd.Program)
	}

	if len(cmd.Args) < 4 {
		t.Errorf("Args length = %d, want at least 4", len(cmd.Args))
		return
	}

	if cmd.Args[0] != "-NoProfile" {
		t.Errorf("Args[0] = %q, want %q", cmd.Args[0], "-NoProfile")
	}

	if cmd.Args[1] != "-NonInteractive" {
		t.Errorf("Args[1] = %q, want %q", cmd.Args[1], "-NonInteractive")
	}

	if cmd.Args[2] != "-Command" {
		t.Errorf("Args[2] = %q, want %q", cmd.Args[2], "-Command")
	}

	// Check script contains expected elements
	script := cmd.Args[3]
	expectedParts := []string{
		"tclaude://focus/abc123",
		"Claude: Idle",
		"abc123 | myproject",
		"ToastNotification",
	}

	for _, part := range expectedParts {
		if !strings.Contains(script, part) {
			t.Errorf("Script should contain %q", part)
		}
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
			if got != tt.want {
				t.Errorf("NotificationBody() = %q, want %q", got, tt.want)
			}
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
			if got != tt.want {
				t.Errorf("NotificationTitle(%q) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestBuildDarwiniTermFocusCmd(t *testing.T) {
	cmd := BuildDarwiniTermFocusCmd("/dev/ttys003")

	if cmd.Program != "osascript" {
		t.Errorf("Program = %q, want %q", cmd.Program, "osascript")
	}

	if len(cmd.Args) != 2 {
		t.Errorf("Args length = %d, want 2", len(cmd.Args))
		return
	}

	script := cmd.Args[1]
	expectedParts := []string{
		"iTerm2",
		"/dev/ttys003",
		"tty of s",
		"select t",
		"select w",
	}

	for _, part := range expectedParts {
		if !strings.Contains(script, part) {
			t.Errorf("Script should contain %q", part)
		}
	}
}

func TestBuildTmuxDetachCmd(t *testing.T) {
	cmd := BuildTmuxDetachCmd("/dev/ttys001")

	if cmd.Program != "tmux" {
		t.Errorf("Program = %q, want %q", cmd.Program, "tmux")
	}

	wantArgs := []string{"-L", "tclaude", "detach-client", "-t", "/dev/ttys001"}
	if len(cmd.Args) != len(wantArgs) {
		t.Errorf("Args = %v, want %v", cmd.Args, wantArgs)
		return
	}

	for i, arg := range cmd.Args {
		if arg != wantArgs[i] {
			t.Errorf("Args[%d] = %q, want %q", i, arg, wantArgs[i])
		}
	}
}

func TestFocusCommandString(t *testing.T) {
	tests := []struct {
		name      string
		clPath  string
		tmuxDir   string
		sessionID string
		wantFunc  func(tmuxDir string) string // dynamic expected value
	}{
		{
			name:      "with tmux dir",
			clPath:  "/usr/bin/tclaude",
			tmuxDir:   "/opt/homebrew/bin",
			sessionID: "abc123",
			wantFunc: func(tmuxDir string) string {
				return fmt.Sprintf("PATH=%s:$PATH /usr/bin/tclaude session focus abc123", filepath.Clean(tmuxDir))
			},
		},
		{
			name:      "without tmux dir",
			clPath:  "/usr/bin/tclaude",
			tmuxDir:   "",
			sessionID: "abc123",
			wantFunc: func(_ string) string {
				return "/usr/bin/tclaude session focus abc123"
			},
		},
		{
			name:      "fallback",
			clPath:  "",
			tmuxDir:   "",
			sessionID: "xyz",
			wantFunc: func(_ string) string {
				return common.DetectCmd() + " session focus xyz"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FocusCommandString(tt.clPath, tt.tmuxDir, tt.sessionID)
			want := tt.wantFunc(tt.tmuxDir)
			if got != want {
				t.Errorf("FocusCommandString() = %q, want %q", got, want)
			}
		})
	}
}
