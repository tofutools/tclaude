//go:build linux

package session

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/wsl"
)

// findPowerShell returns the path to powershell.exe using the shared WSL utilities.
func findPowerShell() string {
	path := wsl.FindPowerShell()
	if path != "" {
		slog.Debug(fmt.Sprintf("Found PowerShell at: %s", path), "module", "focus")
	} else {
		slog.Debug("PowerShell not found", "module", "focus")
	}
	return path
}

// TryFocusAttachedSession attempts to focus the terminal window that has the session attached.
func TryFocusAttachedSession(tmuxSession string) {
	TryFocusAttachedSessionWithID(tmuxSession, os.Getenv("TCLAUDE_SESSION_ID"))
}

// TryFocusAttachedSessionWithID is like TryFocusAttachedSession but
// takes the tclaude session ID (label) explicitly. The session ID is
// needed on WSL where the focus path searches Windows windows by the
// "tclaude:<id>" title pattern that setTerminalTitle stamps on each
// pane. Existing TryFocusAttachedSession reads the ID from
// $TCLAUDE_SESSION_ID, which is correct when called from
// `tclaude session focus` (CLI sets the env first) but not from the
// daemon (env points at the daemon's own session, if any).
func TryFocusAttachedSessionWithID(tmuxSession, sessionID string) {
	slog.Debug(fmt.Sprintf("TryFocusAttachedSessionWithID called for tmux=%s id=%s", tmuxSession, sessionID), "module", "focus")

	if isWSL() {
		slog.Debug("WSL detected, focusing by title pattern", "module", "focus")
		// Skip the wsl.exe parent-tree walk (which is anchored at OUR
		// pid, useless when the daemon is calling for another agent)
		// and go straight to the title-pattern search — same path the
		// notification-click flow uses successfully.
		if sessionID != "" && focusWindowByTitlePattern(sessionID) {
			return
		}
		// Fallback to opening a new WT window attached to the session.
		if sessionID != "" {
			focusWTTabByCycling(sessionID)
		}
		return
	}

	// Native Linux - use xdotool
	slog.Debug("Native Linux, using xdotool", "module", "focus")
	focusLinuxTmuxSession(tmuxSession)
}

// FocusOwnWindow attempts to focus the current process's terminal window.
func FocusOwnWindow() bool {
	slog.Debug("FocusOwnWindow called", "module", "focus")

	if isWSL() {
		slog.Debug("Detected WSL environment", "module", "focus")
		return focusWSLWindow()
	}

	slog.Debug("Native Linux, using xdotool", "module", "focus")
	return focusLinuxCurrentWindow()
}

// GetOwnWindowTitle returns the title of the current terminal window.
func GetOwnWindowTitle() string {
	return ""
}

// isWSL detects if we're running in Windows Subsystem for Linux.
func isWSL() bool {
	return wsl.IsWSL()
}

// focusWSLWindow attempts to focus the terminal window hosting this WSL session.
// It walks up the process tree to find the Windows terminal process and focuses it.
func focusWSLWindow() bool {
	// Get our PID and walk up to find the terminal
	pid := os.Getpid()
	slog.Debug(fmt.Sprintf("Current PID: %d", pid), "module", "focus")

	// Walk up the process tree looking for the init process (PID 1's parent on WSL is the Windows side)
	terminalPID := findTerminalPID(pid)
	slog.Debug(fmt.Sprintf("Found terminal PID: %d", terminalPID), "module", "focus")

	if terminalPID == 0 {
		slog.Debug("No terminal PID found, trying fallback to focus any terminal", "module", "focus")
		// Fallback: just try to focus any Windows Terminal or terminal window
		return focusAnyTerminal()
	}

	return focusWindowByPID(terminalPID)
}

// findTerminalPID walks up the process tree to find the terminal's Windows PID.
// Returns 0 if not found.
func findTerminalPID(pid int) int {
	slog.Debug(fmt.Sprintf("Walking process tree from PID %d", pid), "module", "focus")

	// In WSL, we can try to get the Windows PID from the init process
	// The WSL init process (PID 1) is spawned by the Windows side

	// Walk up to PID 1 (init), then use PowerShell to find its Windows parent
	current := pid
	for current > 1 {
		ppid := getParentPID(current)
		slog.Debug(fmt.Sprintf("  PID %d -> parent %d", current, ppid), "module", "focus")
		if ppid <= 0 {
			break
		}
		current = ppid
	}

	slog.Debug(fmt.Sprintf("Reached PID %d, now querying Windows side", current), "module", "focus")

	// Now use PowerShell to find the Windows process hosting WSL
	// We look for the wsl.exe or WindowsTerminal.exe process
	return getWSLHostPID()
}

// getParentPID returns the parent PID of a process.
func getParentPID(pid int) int {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				ppid, _ := strconv.Atoi(parts[1])
				return ppid
			}
		}
	}
	return 0
}

// getWSLHostPID uses PowerShell to find the Windows process hosting this WSL instance.
// It walks up the process tree from wsl.exe until it finds a process with a window handle.
func getWSLHostPID() int {
	slog.Debug("Querying PowerShell for WSL host process...", "module", "focus")

	// Walk up from wsl.exe until we find a process with a main window
	script := `
$wslProcesses = Get-Process -Name wsl -ErrorAction SilentlyContinue
if (-not $wslProcesses) {
    Write-Output "0|No wsl.exe found"
    exit
}

$wsl = $wslProcesses | Select-Object -First 1
$currentPid = $wsl.Id
$visited = @{}

# Walk up the process tree looking for a window
for ($i = 0; $i -lt 20; $i++) {
    if ($visited[$currentPid]) { break }
    $visited[$currentPid] = $true

    $proc = Get-Process -Id $currentPid -ErrorAction SilentlyContinue
    if ($proc -and $proc.MainWindowHandle -ne [IntPtr]::Zero) {
        Write-Output "$currentPid|Found window: $($proc.ProcessName)"
        exit
    }

    # Get parent
    try {
        $parentPid = (Get-CimInstance Win32_Process -Filter "ProcessId=$currentPid" -ErrorAction SilentlyContinue).ParentProcessId
        if (-not $parentPid -or $parentPid -eq 0) { break }
        Write-Host "  $($proc.ProcessName) ($currentPid) -> parent $parentPid"
        $currentPid = $parentPid
    } catch {
        break
    }
}
Write-Output "0|No window found in process tree"
`
	psPath := findPowerShell()
	if psPath == "" {
		slog.Debug("PowerShell not found", "module", "focus")
		return 0
	}
	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))

	// Parse output - format is "PID|message"
	lines := strings.Split(outStr, "\n")
	for _, line := range lines {
		if strings.Contains(line, "|") {
			parts := strings.SplitN(line, "|", 2)
			pid, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
			if len(parts) > 1 {
				slog.Debug(fmt.Sprintf("PowerShell: %s", parts[1]), "module", "focus")
			}
			if pid > 0 {
				slog.Debug(fmt.Sprintf("PowerShell returned host PID: %d", pid), "module", "focus")
				return pid
			}
		} else if line != "" {
			slog.Debug(fmt.Sprintf("PowerShell trace: %s", line), "module", "focus")
		}
	}

	if err != nil {
		slog.Debug(fmt.Sprintf("PowerShell error: %v", err), "module", "focus")
	}
	return 0
}

// focusWTTab tries to focus a Windows Terminal tab by title.
// Returns true if successfully focused a tab with our session ID.
func focusWTTab(sessionID string) bool {
	slog.Debug(fmt.Sprintf("Trying to focus Windows Terminal tab for session: %s", sessionID), "module", "focus")
	// This is now handled by focusWindowByTitlePattern and focusWTTabByCycling
	return false
}

// focusWTTabByCycling opens a new Windows Terminal window and attaches to the session.
// This is more reliable than trying to find/focus existing tabs.
func focusWTTabByCycling(sessionID string) bool {
	slog.Debug(fmt.Sprintf("Fallback: opening new Windows Terminal window to attach to session %s", sessionID), "module", "focus")

	psPath := findPowerShell()
	if psPath == "" {
		slog.Debug("PowerShell not found", "module", "focus")
		return false
	}

	// Open a new Windows Terminal window with wsl running attach command
	// Use -f (force) to detach from any existing attachment
	// Syntax: wt.exe -w -1 wsl -- bash -c "command" (-w -1 = new window)
	script := fmt.Sprintf(`
wt.exe -w -1 wsl -- bash -lc "tclaude session attach -f %s"
Write-Output "True"
`, sessionID)

	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	slog.Debug(fmt.Sprintf("Open new WT window result: %s", outStr), "module", "focus")

	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to open new WT window: %v", err), "module", "focus")
		return false
	}

	return outStr == "True"
}

// focusWindowByPID focuses a window by its Windows process ID.
// This is now a fallback - we prefer searching ALL windows by title pattern.
func focusWindowByPID(pid int) bool {
	slog.Debug(fmt.Sprintf("Attempting to focus window for PID %d", pid), "module", "focus")

	// First try to find window by our known title pattern (set by setTerminalTitle)
	sessionID := os.Getenv("TCLAUDE_SESSION_ID")
	if sessionID != "" {
		slog.Debug(fmt.Sprintf("Searching ALL windows for title containing 'tclaude:%s'", sessionID), "module", "focus")
		if focusWindowByTitlePattern(sessionID) {
			return true
		}
	}

	slog.Debug("Title pattern search failed, trying tab cycling", "module", "focus")
	if focusWTTabByCycling(sessionID) {
		return true
	}

	slog.Debug("Tab cycling failed, trying PID-based enumeration", "module", "focus")
	return focusWindowByPIDEnumeration(pid, sessionID)
}

// focusWindowByTitlePattern searches ALL visible windows for one matching our title pattern.
// This is more reliable than PID-based search because Windows Terminal tabs all share one process.
func focusWindowByTitlePattern(sessionID string) bool {
	// First try Windows Terminal's native tab focusing via wt.exe
	if focusWTTab(sessionID) {
		return true
	}

	psPath := findPowerShell()
	if psPath == "" {
		slog.Debug("PowerShell not found", "module", "focus")
		return false
	}

	// Search ALL visible windows for our title pattern "tclaude:<sessionID>"
	script := fmt.Sprintf(`
$sessionId = '%s'
$pattern = "tclaude:$sessionId"

Add-Type @"
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using System.Text;

public class AllWindowEnumerator {
    public delegate bool EnumWindowsProc(IntPtr hWnd, IntPtr lParam);

    [DllImport("user32.dll")]
    public static extern bool EnumWindows(EnumWindowsProc lpEnumFunc, IntPtr lParam);

    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    public static extern int GetWindowText(IntPtr hWnd, StringBuilder lpString, int nMaxCount);

    [DllImport("user32.dll")]
    public static extern bool IsWindowVisible(IntPtr hWnd);

    public static List<string> GetAllVisibleWindowTitles() {
        var titles = new List<string>();
        EnumWindows((hWnd, lParam) => {
            if (IsWindowVisible(hWnd)) {
                StringBuilder sb = new StringBuilder(512);
                GetWindowText(hWnd, sb, 512);
                string title = sb.ToString();
                if (!string.IsNullOrEmpty(title)) {
                    titles.Add(title);
                }
            }
            return true;
        }, IntPtr.Zero);
        return titles;
    }
}
"@

$titles = [AllWindowEnumerator]::GetAllVisibleWindowTitles()
Write-Host "Searching $($titles.Count) visible windows for pattern: $pattern"

foreach ($title in $titles) {
    Write-Host "  Checking: $title"
    if ($title -like "*$pattern*") {
        Write-Host "FOUND MATCH!"
        Write-Output "MATCH|$title"
        exit 0
    }
}

Write-Host "No matching window found"
Write-Output "NOMATCH"
`, sessionID)

	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	slog.Debug(fmt.Sprintf("Title pattern search output:\n%s", outStr), "module", "focus")

	if err != nil {
		slog.Debug(fmt.Sprintf("Title pattern search error: %v", err), "module", "focus")
	}

	// Parse output
	for _, line := range strings.Split(outStr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "MATCH|") {
			matchTitle := strings.TrimPrefix(line, "MATCH|")
			slog.Debug(fmt.Sprintf("Found matching window: %s", matchTitle), "module", "focus")
			return focusByTitle(matchTitle)
		}
	}

	return false
}

// focusWindowByPIDEnumeration is the fallback that enumerates windows for a specific PID.
func focusWindowByPIDEnumeration(pid int, sessionID string) bool {
	psPath := findPowerShell()
	if psPath == "" {
		slog.Debug("PowerShell not found", "module", "focus")
		return false
	}

	// Enumerate all windows for this process and find one with our session ID in title
	script := fmt.Sprintf(`
$targetPid = %d
$sessionId = '%s'

Add-Type @"
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using System.Text;

public class WindowEnumerator {
    public delegate bool EnumWindowsProc(IntPtr hWnd, IntPtr lParam);

    [DllImport("user32.dll")]
    public static extern bool EnumWindows(EnumWindowsProc lpEnumFunc, IntPtr lParam);

    [DllImport("user32.dll")]
    public static extern uint GetWindowThreadProcessId(IntPtr hWnd, out uint lpdwProcessId);

    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    public static extern int GetWindowText(IntPtr hWnd, StringBuilder lpString, int nMaxCount);

    [DllImport("user32.dll")]
    public static extern bool IsWindowVisible(IntPtr hWnd);

    public static List<Tuple<IntPtr, string>> GetWindowsForProcess(uint targetPid) {
        var windows = new List<Tuple<IntPtr, string>>();
        EnumWindows((hWnd, lParam) => {
            uint pid;
            GetWindowThreadProcessId(hWnd, out pid);
            if (pid == targetPid && IsWindowVisible(hWnd)) {
                StringBuilder sb = new StringBuilder(512);
                GetWindowText(hWnd, sb, 512);
                string title = sb.ToString();
                if (!string.IsNullOrEmpty(title)) {
                    windows.Add(Tuple.Create(hWnd, title));
                }
            }
            return true;
        }, IntPtr.Zero);
        return windows;
    }
}
"@

$windows = [WindowEnumerator]::GetWindowsForProcess(%d)
Write-Host "Found $($windows.Count) windows for PID %d"

foreach ($w in $windows) {
    $handle = $w.Item1
    $title = $w.Item2
    Write-Host "  Window: $title (handle: $handle)"

    # Check if this window's title contains our session ID
    if ($title -match $sessionId -or $title -match 'TCLAUDE_SESSION_ID' -or $title -like "*tclaude:*") {
        Write-Host "Found matching window!"
        Write-Output "MATCH|$title"
        exit 0
    }
}

# If no exact match, output all titles for debugging
foreach ($w in $windows) {
    Write-Output "WINDOW|$($w.Item2)"
}
`, pid, sessionID, pid, pid)

	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(string(out))
	slog.Debug(fmt.Sprintf("Window enumeration output:\n%s", outStr), "module", "focus")

	// Parse output to find matching window title
	var matchTitle string
	var allTitles []string
	for _, line := range strings.Split(outStr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "MATCH|") {
			matchTitle = strings.TrimPrefix(line, "MATCH|")
			break
		} else if strings.HasPrefix(line, "WINDOW|") {
			allTitles = append(allTitles, strings.TrimPrefix(line, "WINDOW|"))
		}
	}

	if matchTitle != "" {
		slog.Debug(fmt.Sprintf("Focusing matched window: %s", matchTitle), "module", "focus")
		return focusByTitle(matchTitle)
	}

	// No exact match - log all titles found
	slog.Debug(fmt.Sprintf("No matching window found. Available windows: %v", allTitles), "module", "focus")

	if err != nil {
		slog.Debug(fmt.Sprintf("Enumeration error: %v", err), "module", "focus")
	}

	return false
}

// focusByTitle focuses a window using AppActivate by title
func focusByTitle(title string) bool {
	slog.Debug(fmt.Sprintf("Focusing by title: %s", title), "module", "focus")

	// Escape quotes in title
	escapedTitle := strings.ReplaceAll(title, "'", "''")

	script := fmt.Sprintf(`
$title = '%s'
$wshell = New-Object -ComObject wscript.shell
$wshell.AppActivate($title)
`, escapedTitle)

	psPath := findPowerShell()
	if psPath == "" {
		return false
	}

	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script)
	output, err := cmd.CombinedOutput()
	if len(output) > 0 {
		slog.Debug(fmt.Sprintf("AppActivate output: %s", strings.TrimSpace(string(output))), "module", "focus")
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("AppActivate failed: %v", err), "module", "focus")
		return false
	}
	slog.Debug("AppActivate succeeded", "module", "focus")
	return true
}

// focusAnyTerminal tries to focus Windows Terminal or other common terminals.
func focusAnyTerminal() bool {
	slog.Debug("Trying to focus any terminal window...", "module", "focus")

	// Try common terminal process names
	terminals := []string{"WindowsTerminal", "cmd", "powershell", "pwsh", "ConEmu64", "ConEmu"}
	slog.Debug(fmt.Sprintf("Looking for: %v", terminals), "module", "focus")

	script := fmt.Sprintf(`
$ProcessNames = "%s"
Add-Type @"
using System;
using System.Runtime.InteropServices;
public class WindowHelper {
    [DllImport("user32.dll")]
    public static extern bool SetForegroundWindow(IntPtr hWnd);
    [DllImport("user32.dll")]
    public static extern bool ShowWindow(IntPtr hWnd, int nCmdShow);
    [DllImport("user32.dll")]
    public static extern bool IsIconic(IntPtr hWnd);

    public const int SW_RESTORE = 9;

    public static bool FocusWindow(IntPtr hWnd) {
        if (IsIconic(hWnd)) {
            ShowWindow(hWnd, SW_RESTORE);
        }
        return SetForegroundWindow(hWnd);
    }
}
"@

$names = $ProcessNames -split ','
foreach ($name in $names) {
    $procs = Get-Process -Name $name -ErrorAction SilentlyContinue
    Write-Host "Checking $name : found $($procs.Count) processes"
    foreach ($proc in $procs) {
        if ($proc.MainWindowHandle -ne [IntPtr]::Zero) {
            Write-Host "Focusing $($proc.ProcessName) PID $($proc.Id) handle $($proc.MainWindowHandle)"
            $result = [WindowHelper]::FocusWindow($proc.MainWindowHandle)
            Write-Host "SetForegroundWindow returned: $result"
            exit 0
        }
    }
}
Write-Host "No terminal with window handle found"
exit 1
`, strings.Join(terminals, ","))
	psPath := findPowerShell()
	if psPath == "" {
		slog.Debug("PowerShell not found", "module", "focus")
		return false
	}
	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", script)
	output, err := cmd.CombinedOutput()
	if len(output) > 0 {
		slog.Debug(fmt.Sprintf("PowerShell output: %s", strings.TrimSpace(string(output))), "module", "focus")
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("Focus any terminal failed: %v", err), "module", "focus")
		return false
	}
	slog.Debug("Focus any terminal succeeded", "module", "focus")
	return true
}

// =============================================================================
// Native Linux Focus Support (xdotool / kdotool)
// =============================================================================
//
// Two interchangeable focus tools cover the two display servers Linux
// hosts in practice:
//
//   - xdotool: X11-only. Works under X11 sessions and X11 apps running
//     through XWayland; reports no windows for native-Wayland apps.
//   - kdotool: KDE Plasma's xdotool-compatible bridge. Generates KWin
//     scripts on the fly via DBus, so it works under both X11 and
//     Wayland Plasma sessions — including for the konsole windows
//     Kubuntu users have. Its `search --pid` / `search --name` /
//     `windowactivate` surface is the subset we use, with one
//     difference: kdotool's `windowactivate` does not accept `--sync`.
//     Window IDs are KWin UUIDs rather than X11 IDs, but each tool's
//     IDs are valid in its own activate call — we use one tool end to
//     end per call chain.
//
// On Wayland sessions xdotool returns success-but-empty for `search`
// (the historical behaviour that made the dashboard's focus button
// look like a no-op for Kubuntu/KDE users), so the dispatcher prefers
// kdotool whenever WAYLAND_DISPLAY is set and falls back to xdotool
// when only X11 is available. If only one is installed, that one is
// used regardless of session type.

// resolveLinuxFocusToolOnce caches the focus-tool detection. The
// answer is process-stable for the lifetime of agentd (display server +
// installed tools rarely change underneath us), so paying for the
// exec.LookPath / env reads once is enough.
var (
	resolveLinuxFocusToolOnce sync.Once
	cachedLinuxFocusTool      string
)

// pickPreferredFocusTool returns ("preferred", "fallback") given the
// display-server env vars, with the preferred tool chosen for the
// session type: a Wayland-only session (WAYLAND_DISPLAY set, DISPLAY
// unset) prefers kdotool because xdotool cannot see native-Wayland
// windows; everywhere else xdotool wins because it is older and more
// battle-tested. The fallback is whichever wasn't chosen — used when
// the preferred binary is missing. Pure so the env→preference table
// can be unit-tested without faking exec.LookPath.
func pickPreferredFocusTool(display, wayland string) (preferred, fallback string) {
	if wayland != "" && display == "" {
		return "kdotool", "xdotool"
	}
	return "xdotool", "kdotool"
}

// focusLookPath is the exec.LookPath seam for tests. Production points
// at the real exec.LookPath; tests pass a fake that pins which focus
// tools are "installed".
var focusLookPath = exec.LookPath

// resolveLinuxFocusTool returns the name of the focus binary the
// per-call helpers below should drive — "xdotool", "kdotool", or "" if
// neither is installed. The choice combines pickPreferredFocusTool's
// env-aware preference with the installed-set check.
func resolveLinuxFocusTool() string {
	resolveLinuxFocusToolOnce.Do(func() {
		cachedLinuxFocusTool = chooseLinuxFocusTool(
			os.Getenv("DISPLAY"), os.Getenv("WAYLAND_DISPLAY"), focusLookPath)
	})
	return cachedLinuxFocusTool
}

// chooseLinuxFocusTool is resolveLinuxFocusTool factored to take the
// env values and a lookPath seam as args, so the full preferred/fallback
// resolution is unit-testable.
func chooseLinuxFocusTool(display, wayland string, lookPath func(string) (string, error)) string {
	preferred, fallback := pickPreferredFocusTool(display, wayland)
	if _, err := lookPath(preferred); err == nil {
		return preferred
	}
	if _, err := lookPath(fallback); err == nil {
		return fallback
	}
	return ""
}

// windowActivateCmd builds the activate-by-id command for the chosen
// tool. xdotool gets the --sync that makes the activation wait for the
// X server round-trip; kdotool does not accept --sync and is
// synchronous through its KWin DBus call anyway.
func windowActivateCmd(tool, windowID string) *exec.Cmd {
	if tool == "xdotool" {
		return exec.Command(tool, "windowactivate", "--sync", windowID)
	}
	return exec.Command(tool, "windowactivate", windowID)
}

// focusLinuxTmuxSession focuses the terminal window running a specific tmux session.
func focusLinuxTmuxSession(tmuxSession string) bool {
	tool := resolveLinuxFocusTool()
	if tool == "" {
		slog.Debug("no focus tool found (xdotool / kdotool), cannot focus window", "module", "focus")
		return false
	}

	// Get the client TTY for this tmux session
	cmd := clcommon.TmuxCommand("list-clients", "-t", tmuxSession, "-F", "#{client_tty}")
	output, err := cmd.Output()
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to get tmux client tty: %v", err), "module", "focus")
		return false
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		slog.Debug("No clients attached to tmux session", "module", "focus")
		return false
	}
	tty := lines[0]
	slog.Debug(fmt.Sprintf("Found client TTY: %s (tool=%s)", tty, tool), "module", "focus")

	// Find the terminal window by walking the process tree from TTY
	return focusLinuxWindowByTTY(tool, tty)
}

// focusLinuxCurrentWindow focuses the current terminal window.
func focusLinuxCurrentWindow() bool {
	tool := resolveLinuxFocusTool()
	if tool == "" {
		slog.Debug("no focus tool found (xdotool / kdotool), cannot focus window", "module", "focus")
		return false
	}

	// Try to get the active window from the current TTY
	tty, err := os.Readlink("/proc/self/fd/0")
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to get current TTY: %v", err), "module", "focus")
		return false
	}
	slog.Debug(fmt.Sprintf("Current TTY: %s (tool=%s)", tty, tool), "module", "focus")

	return focusLinuxWindowByTTY(tool, tty)
}

// focusLinuxWindowByTTY finds and focuses the terminal window owning a TTY.
func focusLinuxWindowByTTY(tool, tty string) bool {
	// Find processes on this TTY
	cmd := exec.Command("lsof", "-t", tty)
	output, err := cmd.Output()
	if err != nil {
		slog.Debug(fmt.Sprintf("lsof failed for TTY %s: %v", tty, err), "module", "focus")
		// Fallback: try to focus by window name pattern
		return focusLinuxWindowByPattern(tool, "tclaude:")
	}

	pids := strings.Fields(string(output))
	slog.Debug(fmt.Sprintf("Found PIDs on TTY: %v", pids), "module", "focus")

	// Walk up process tree to find terminal
	for _, pidStr := range pids {
		if windowID := findLinuxWindowForPID(tool, pidStr); windowID != "" {
			return focusLinuxWindowByID(tool, windowID)
		}
	}

	// Fallback: try by window name
	return focusLinuxWindowByPattern(tool, "tclaude:")
}

// findLinuxWindowForPID walks up the process tree to find a window.
func findLinuxWindowForPID(tool, pidStr string) string {
	visited := make(map[string]bool)
	current := pidStr

	for current != "" && current != "0" && current != "1" && !visited[current] {
		visited[current] = true

		// Try to find a window for this PID via the active focus tool
		cmd := exec.Command(tool, "search", "--pid", current)
		output, err := cmd.Output()
		if err == nil {
			windows := strings.Fields(string(output))
			if len(windows) > 0 {
				slog.Debug(fmt.Sprintf("Found window %s for PID %s", windows[0], current), "module", "focus")
				return windows[0]
			}
		}

		// Get parent PID
		ppidData, err := os.ReadFile("/proc/" + current + "/status")
		if err != nil {
			break
		}
		for _, line := range strings.Split(string(ppidData), "\n") {
			if strings.HasPrefix(line, "PPid:") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					current = parts[1]
					break
				}
			}
		}
	}

	return ""
}

// focusLinuxWindowByID focuses a window by its window ID.
func focusLinuxWindowByID(tool, windowID string) bool {
	slog.Debug(fmt.Sprintf("Focusing window ID: %s (tool=%s)", windowID, tool), "module", "focus")

	cmd := windowActivateCmd(tool, windowID)
	if err := cmd.Run(); err != nil {
		slog.Debug(fmt.Sprintf("%s windowactivate failed: %v", tool, err), "module", "focus")
		return false
	}

	slog.Debug("Successfully focused window", "module", "focus")
	return true
}

// focusLinuxWindowByPattern searches for and focuses a window by name pattern.
func focusLinuxWindowByPattern(tool, pattern string) bool {
	slog.Debug(fmt.Sprintf("Searching for window with pattern: %s (tool=%s)", pattern, tool), "module", "focus")

	// Search for windows matching the pattern
	cmd := exec.Command(tool, "search", "--name", pattern)
	output, err := cmd.Output()
	if err != nil {
		slog.Debug(fmt.Sprintf("%s search failed: %v", tool, err), "module", "focus")
		return false
	}

	windows := strings.Fields(string(output))
	if len(windows) == 0 {
		slog.Debug("No windows found matching pattern", "module", "focus")
		return false
	}

	slog.Debug(fmt.Sprintf("Found %d windows, focusing first: %s", len(windows), windows[0]), "module", "focus")
	return focusLinuxWindowByID(tool, windows[0])
}

// IsXdotoolInstalled checks if xdotool is available.
func IsXdotoolInstalled() bool {
	_, err := exec.LookPath("xdotool")
	return err == nil
}

// IsKdotoolInstalled checks if kdotool is available — the KDE Plasma
// Wayland-friendly xdotool replacement.
func IsKdotoolInstalled() bool {
	_, err := exec.LookPath("kdotool")
	return err == nil
}

// LinuxFocusToolName reports the focus binary the dispatcher will use,
// or "" if neither xdotool nor kdotool is installed. For setup-time
// diagnostics.
func LinuxFocusToolName() string {
	return resolveLinuxFocusTool()
}
