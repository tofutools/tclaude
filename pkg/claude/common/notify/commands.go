// Package notify provides OS notifications for session state transitions.
// This file contains command builders that can be unit tested.
package notify

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common"
)

// DarwinNotifyCmd represents the command to Send a notification on macOS.
type DarwinNotifyCmd struct {
	Program string
	Args    []string
}

// BuildDarwinNotifyCmd builds the terminal-notifier command for macOS.
// clPath and tmuxDir can be empty strings if not available.
func BuildDarwinNotifyCmd(title, body, sessionID, clPath, tmuxDir string) DarwinNotifyCmd {
	// Build the focus command that runs when notification is clicked
	var focusCmd string
	if clPath == "" {
		clPath = common.DetectCmd()
	}
	if tmuxDir != "" {
		focusCmd = fmt.Sprintf("PATH=%s:$PATH %s session focus %s",
			tmuxDir, clPath, sessionID)
	} else {
		focusCmd = fmt.Sprintf("%s session focus %s", clPath, sessionID)
	}

	return DarwinNotifyCmd{
		Program: "terminal-notifier",
		Args: []string{
			"-title", title,
			"-message", body,
			"-execute", focusCmd,
			"-sound", "default",
		},
	}
}

// BuildDarwinFallbackCmd builds the osascript fallback command for macOS
// when terminal-notifier is not available.
func BuildDarwinFallbackCmd(title, body string) DarwinNotifyCmd {
	script := fmt.Sprintf(`display notification "%s" with title "%s"`,
		strings.ReplaceAll(body, "\"", "\\\""),
		strings.ReplaceAll(title, "\"", "\\\""))

	return DarwinNotifyCmd{
		Program: "osascript",
		Args:    []string{"-e", script},
	}
}

// WSLNotifyCmd represents the command to Send a notification on WSL.
type WSLNotifyCmd struct {
	Program string
	Args    []string
}

// BuildWSLNotifyCmd builds the PowerShell command for WSL toast notifications.
// psPath is the path to PowerShell, protocolURL is the tclaude:// URL to open on click.
func BuildWSLNotifyCmd(title, body, psPath, protocolURL string) WSLNotifyCmd {
	// Escape for PowerShell
	escapedTitle := strings.ReplaceAll(title, "'", "''")
	escapedBody := strings.ReplaceAll(body, "'", "''")

	// Build the PowerShell script for toast notification
	script := fmt.Sprintf(`
$xml = @"
<toast activationType="protocol" launch="%s">
  <visual>
    <binding template="ToastGeneric">
      <text>%s</text>
      <text>%s</text>
    </binding>
  </visual>
  <audio src="ms-winsoundevent:Notification.Default"/>
</toast>
"@
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null
$toastXml = New-Object Windows.Data.Xml.Dom.XmlDocument
$toastXml.LoadXml($xml)
$toast = New-Object Windows.UI.Notifications.ToastNotification $toastXml
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('tclaude').Show($toast)
`, protocolURL, escapedTitle, escapedBody)

	return WSLNotifyCmd{
		Program: psPath,
		Args:    []string{"-NoProfile", "-NonInteractive", "-Command", script},
	}
}

// FocusCmd represents a command to focus a terminal window.
type FocusCmd struct {
	Program string
	Args    []string
}

// BuildDarwinFocusCmd builds the AppleScript command to focus a terminal app.
func BuildDarwinFocusCmd(terminalApp string) FocusCmd {
	script := fmt.Sprintf(`tell application "%s" to activate`, terminalApp)
	return FocusCmd{
		Program: "osascript",
		Args:    []string{"-e", script},
	}
}

// BuildDarwiniTermFocusCmd builds the AppleScript to focus a specific iTerm2 tab by TTY.
func BuildDarwiniTermFocusCmd(tty string) FocusCmd {
	script := fmt.Sprintf(`
tell application "iTerm2"
	activate
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is "%s" then
					select t
					select w
					return true
				end if
			end repeat
		end repeat
	end repeat
	return false
end tell
`, tty)
	return FocusCmd{
		Program: "osascript",
		Args:    []string{"-e", script},
	}
}

// NotificationBody builds the notification body text.
func NotificationBody(sessionID, projectName, convTitle string) string {
	shortID := sessionID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}

	if convTitle != "" {
		return fmt.Sprintf("%s | %s - %s", shortID, projectName, convTitle)
	}
	return fmt.Sprintf("%s | %s", shortID, projectName)
}

// NotificationTitle builds the notification title.
func NotificationTitle(status string) string {
	// Map internal status to display format
	display := status
	switch status {
	case "idle":
		display = "Idle"
	case "working":
		display = "Working"
	case "awaiting_permission":
		display = "Awaiting permission"
	case "awaiting_input":
		display = "Awaiting input"
	case "exited":
		display = "Exited"
	}
	return fmt.Sprintf("Claude: %s", display)
}

// FocusCommandString builds the shell command string for focusing a session.
func FocusCommandString(clPath, tmuxDir, sessionID string) string {
	if clPath == "" {
		clPath = common.DetectCmd()
	}
	if tmuxDir != "" {
		return fmt.Sprintf("PATH=%s:$PATH %s session focus %s",
			filepath.Clean(tmuxDir), clPath, sessionID)
	}
	return fmt.Sprintf("%s session focus %s", clPath, sessionID)
}
