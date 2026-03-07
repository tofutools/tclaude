package notify

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"

	"github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/wsl"
)

// platformSend sends a notification using Linux-specific methods.
// It checks for WSL at runtime and dispatches accordingly.
func platformSend(sessionID, title, body string) error {
	if wsl.IsWSL() {
		return sendWSLClickable(sessionID, title, body)
	}
	return sendLinuxClickable(sessionID, title, body)
}

// sendLinuxClickable sends a notification with click-to-focus on native Linux via D-Bus.
// It spawns a detached background process that sends the notification and listens for
// the click action on the same D-Bus connection. This is necessary because:
// 1. The hook-callback process exits immediately, so goroutines can't be used
// 2. D-Bus notification daemons send ActionInvoked signals only to the connection
//
//	that created the notification, so send and listen must share a connection
func sendLinuxClickable(sessionID, title, body string) error {
	clArgs := common.DetectArgs()
	listenerArgs := append(clArgs[1:], "session", "notify-listen", sessionID, title, body)
	listener := exec.Command(clArgs[0], listenerArgs...)
	listener.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := listener.Start(); err != nil {
		return fmt.Errorf("failed to start notify listener: %w", err)
	}
	// Detach - don't wait for the listener process
	go listener.Wait()

	return nil
}

// sendWSLClickable sends a Windows Toast notification that focuses the terminal on click.
// Note: Requires 'tclaude setup' to have been run to register the protocol handler.
// If not registered, the notification still shows but clicking won't focus the terminal.
func sendWSLClickable(sessionID, title, body string) error {
	return notifyWSLClickable(title, body, sessionID)
}

// notifyWSLClickable sends a Windows Toast notification that runs a command on click.
func notifyWSLClickable(title, body, sessionID string) error {
	psPath := wsl.FindPowerShell()
	if psPath == "" {
		return fmt.Errorf("powershell not found")
	}

	// Escape for XML
	title = escapeXML(title)
	body = escapeXML(body)

	// PowerShell script for clickable Windows Toast notification
	// Uses protocol activation to trigger tclaude://focus/SESSION_ID
	// Uses Windows Terminal's AppUserModelID for a nicer notification appearance
	script := fmt.Sprintf(`
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null

$template = @'
<toast activationType="protocol" launch="tclaude://focus/%s">
  <visual>
    <binding template="ToastGeneric">
      <text>%s</text>
      <text>%s</text>
      <text placement="attribution">Click to focus terminal</text>
    </binding>
  </visual>
</toast>
'@

$xml = New-Object Windows.Data.Xml.Dom.XmlDocument
$xml.LoadXml($template)
$toast = [Windows.UI.Notifications.ToastNotification]::new($xml)

# Try to use Windows Terminal's AppUserModelID for nicer appearance
$appId = 'Microsoft.WindowsTerminal_8wekyb3d8bbwe!App'
try {
    [Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($appId).Show($toast)
} catch {
    # Fallback to generic notifier
    [Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('tclaude').Show($toast)
}
`, sessionID, title, body)

	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script)
	return cmd.Run()
}

// escapeXML escapes special characters for XML content.
func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

// notifyWSL sends a Windows Toast notification via PowerShell from WSL (non-clickable fallback).
func notifyWSL(title, body string) error {
	psPath := wsl.FindPowerShell()
	if psPath == "" {
		return fmt.Errorf("powershell not found")
	}

	// Escape single quotes for PowerShell
	title = strings.ReplaceAll(title, "'", "''")
	body = strings.ReplaceAll(body, "'", "''")

	// PowerShell script for Windows Toast notification
	script := fmt.Sprintf(`
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null
$template = @'
<toast>
  <visual>
    <binding template="ToastText02">
      <text id="1">%s</text>
      <text id="2">%s</text>
    </binding>
  </visual>
</toast>
'@
$xml = New-Object Windows.Data.Xml.Dom.XmlDocument
$xml.LoadXml($template)
$toast = [Windows.UI.Notifications.ToastNotification]::new($xml)
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('tclaude').Show($toast)
`, title, body)

	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script)
	return cmd.Run()
}
