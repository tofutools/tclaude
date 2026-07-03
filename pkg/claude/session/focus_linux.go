//go:build linux

package session

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/terminal"
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

	// raiseOnly gates the open-a-fresh-window fallbacks below. Default
	// false (open-on-focus); the focus.raise_only config opt-in flips it.
	raiseOnly := focusRaiseOnlyFn()

	if isWSLFn() {
		slog.Debug("WSL detected, focusing by title pattern", "module", "focus")
		// Skip the wsl.exe parent-tree walk (which is anchored at OUR
		// pid, useless when the daemon is calling for another agent)
		// and go straight to the title-pattern search — same path the
		// notification-click flow uses successfully.
		if sessionID != "" && focusWindowByTitlePattern(sessionID) {
			return
		}
		// Fallback to opening a new WT window attached to the session —
		// unless raise-only is configured, in which case there is no
		// existing window to raise and we stop here.
		if !raiseOnly && sessionID != "" {
			focusWTTabByCycling(sessionID)
		}
		return
	}

	// Native Linux. The inner helpers log the resolved focus tool
	// themselves ("Found client TTY: … (tool=xdotool|kdotool)"); a
	// fixed "using xdotool" banner up here would be misleading now
	// that kdotool is in the dispatch.
	switch r := focusLinuxTmuxSessionFn(tmuxSession); r {
	case focusLinuxFocused:
		return
	case focusLinuxNoClients:
		// Genuinely nothing to focus. By default we open a fresh terminal
		// that runs `tclaude session attach` so the human gets a window
		// without launching one by hand (the no-client case PR #201 added
		// the fallback for). With focus.raise_only configured we stop here
		// instead — there is no existing window to raise, and the user has
		// opted out of the open-on-focus side effect (use the explicit
		// dashboard "open window" action to get a console).
		switch {
		case raiseOnly:
			slog.Debug("no client window to raise; focus.raise_only set, not opening a new terminal",
				"tmux", tmuxSession, "id", sessionID, "module", "focus")
		case sessionID != "":
			openLinuxAttachTerminal(sessionID)
		}
	case focusLinuxTryFailed:
		// Either no focus tool installed, tmux list-clients errored,
		// or a client IS attached but activate returned non-zero. We
		// cannot safely fall back to opening a new terminal — doing so
		// would risk giving the human two attached clients to the
		// same tmux session. Log warn so the user can see the focus
		// attempt happened; the inner helpers have already logged the
		// specific failure reason at debug.
		slog.Warn("could not focus attached tmux session; not spawning a new terminal to avoid duplicate-client attach",
			"tmux", tmuxSession, "id", sessionID, "module", "focus")
	default:
		// A future focusLinuxResult variant — surface noisily rather
		// than silently picking one of the existing branches'
		// behaviours by accident. Bare-zero (focusLinuxUnknown) hits
		// here too, which is exactly the regression this sentinel +
		// arm is paired against.
		slog.Error("TryFocusAttachedSessionWithID: unknown focusLinuxResult; not spawning",
			"result", int(r), "tmux", tmuxSession, "id", sessionID, "module", "focus")
	}
}

// FocusOwnWindow attempts to focus the current process's terminal window.
func FocusOwnWindow() bool {
	slog.Debug("FocusOwnWindow called", "module", "focus")

	if isWSL() {
		slog.Debug("Detected WSL environment", "module", "focus")
		return focusWSLWindow()
	}

	// Same rationale as TryFocusAttachedSessionWithID: skip the
	// "using xdotool" banner — the inner helper logs the resolved
	// tool, which may be kdotool on Wayland Plasma.
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

// isWSLFn is the WSL-detection seam used by TryFocusAttachedSessionWithID.
// Production points at the real /proc/version probe; tests swap to
// return false so the native-Linux orchestration branch can be
// exercised even when the test host IS WSL — yamzz (the human)
// develops on WSL2, and a test that only runs under CI's
// ubuntu-latest fails to guard the dev loop where the regression
// would land first. (The other tclaude isWSL call sites here are
// internal-to-WSL helpers — focusWSLWindow / findTerminalPID — that
// only run AFTER the WSL branch is taken, so threading the seam past
// this entry-point check is unnecessary.)
var isWSLFn = isWSL

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
// kdotool — BUT ONLY when the session is actually KDE. kdotool fails
// fast with "Unsupported KDE version" on GNOME / Sway / Hyprland, so
// preferring it everywhere on Wayland would degrade those desktops
// from "xdotool works for XWayland apps" to "kdotool errors and the
// xdotool fallback runs anyway". The session-type check uses
// XDG_CURRENT_DESKTOP (containing "KDE") OR KDE_SESSION_VERSION
// (non-empty) — both are set by KDE Plasma's session start-up.

// focusLookPath is the exec.LookPath seam for tests. Production points
// at the real exec.LookPath; tests pass a fake that pins which focus
// tools are "installed".
var focusLookPath = exec.LookPath

// isKDESession reports whether the current login session is KDE
// Plasma, given the two env vars KDE sets. Pure so the gate in
// pickPreferredFocusTool can be unit-tested without t.Setenv.
func isKDESession(currentDesktop, kdeSessionVersion string) bool {
	if kdeSessionVersion != "" {
		return true
	}
	for _, part := range strings.Split(currentDesktop, ":") {
		if strings.EqualFold(strings.TrimSpace(part), "KDE") {
			return true
		}
	}
	return false
}

// pickPreferredFocusTool returns ("preferred", "fallback") given the
// display-server env vars + the KDE-session bits, with the preferred
// tool chosen for the session type:
//
//   - KDE Plasma Wayland (WAYLAND_DISPLAY set + KDE detected) → kdotool
//     wins, because xdotool cannot see native-Wayland Plasma windows.
//   - Everywhere else → xdotool wins, because it is older + more
//     battle-tested, AND kdotool refuses to run on non-KDE desktops
//     (upstream main.rs:626 bails with "Unsupported KDE version" when
//     KDE_SESSION_VERSION != "6", so preferring it on GNOME/Sway/
//     Hyprland would just produce errors and force the xdotool
//     fallback anyway).
//
// The fallback is whichever wasn't chosen — used when the preferred
// binary is missing. Pure so the env→preference table can be
// unit-tested without faking exec.LookPath.
func pickPreferredFocusTool(display, wayland, currentDesktop, kdeSessionVersion string) (preferred, fallback string) {
	_ = display // future: an XWayland-only KDE session may want different routing
	if wayland != "" && isKDESession(currentDesktop, kdeSessionVersion) {
		return "kdotool", "xdotool"
	}
	return "xdotool", "kdotool"
}

// resolveLinuxFocusTool returns the name of the focus binary the
// per-call helpers below should drive — "xdotool", "kdotool", or "" if
// neither is installed. The choice combines pickPreferredFocusTool's
// env-aware preference with the installed-set check.
//
// Not cached: focus dispatch is rare (per dashboard button click),
// exec.LookPath is microseconds, and the previous sync.Once cache
// could lock in "no tool" if agentd happened to start before the
// user's graphical session was up or before xdotool/kdotool was
// installed. Cheap to re-resolve.
func resolveLinuxFocusTool() string {
	return chooseLinuxFocusTool(
		os.Getenv("DISPLAY"),
		os.Getenv("WAYLAND_DISPLAY"),
		os.Getenv("XDG_CURRENT_DESKTOP"),
		os.Getenv("KDE_SESSION_VERSION"),
		focusLookPath,
	)
}

// chooseLinuxFocusTool is resolveLinuxFocusTool factored to take the
// env values and a lookPath seam as args, so the full preferred/fallback
// resolution is unit-testable.
func chooseLinuxFocusTool(display, wayland, currentDesktop, kdeSessionVersion string, lookPath func(string) (string, error)) string {
	preferred, fallback := pickPreferredFocusTool(display, wayland, currentDesktop, kdeSessionVersion)
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

// focusLinuxTmuxSessionFn is the focus-dispatch seam used by
// TryFocusAttachedSessionWithID. Production points at the real
// focusLinuxTmuxSession; tests swap it to return a chosen
// focusLinuxResult so the orchestration (which outcome triggers the
// open-fresh-terminal fallback) is unit-testable without a live tmux,
// a live focus tool, or a real desktop session.
var focusLinuxTmuxSessionFn = focusLinuxTmuxSession

// focusLinuxResult distinguishes the three outcomes the focus dispatch
// can reach, so TryFocusAttachedSessionWithID can gate the
// open-fresh-terminal fallback on the only outcome where opening a new
// window is the right answer.
//
//   - focusLinuxFocused: a window was found and activate succeeded.
//     The caller is done.
//   - focusLinuxNoClients: tmux confirmed there are no clients
//     attached to this session — nobody to focus. Opening a fresh
//     terminal that runs `tclaude session attach` is exactly what the
//     dashboard's focus semantics ask for in this case (see
//     window_focus.go's "focus" docstring).
//   - focusLinuxTryFailed: we cannot tell whether anyone is attached,
//     OR we know they are but activate failed. Opening a fresh
//     terminal here would risk handing the human a SECOND client to
//     the same tmux session — `tmux attach` is multiplexed, so two
//     attached clients is a real (and surprising) state. Match macOS,
//     which gates on `tty == ""` (focus_darwin.go:44) and never
//     spawns when the focus attempt could plausibly have raced with
//     an existing window.
type focusLinuxResult int

const (
	// focusLinuxUnknown sits at iota=0 deliberately. A
	// `var r focusLinuxResult` or a bare `return` from a future
	// refactor would otherwise implicitly mean "focused" — the most
	// dangerous default, since the caller would skip the
	// noClients-spawn fallback entirely AND skip the tryFailed warn.
	// Reserving zero as a noisy sentinel lets the default arm in
	// TryFocusAttachedSessionWithID catch the accident before it
	// reaches a user.
	focusLinuxUnknown focusLinuxResult = iota
	focusLinuxFocused
	focusLinuxNoClients
	focusLinuxTryFailed
)

// focusLinuxTmuxSession focuses the terminal window running a specific
// tmux session and reports which of the three outcomes happened.
//
// "no focus tool installed" maps to tryFailed (NOT noClients) because
// at that point we have not been able to ask tmux about clients —
// spawning could duplicate one. Same reasoning for a tmux list-clients
// error and a per-window focusLinuxWindowByTTY failure.
func focusLinuxTmuxSession(tmuxSession string) focusLinuxResult {
	tool := resolveLinuxFocusTool()
	if tool == "" {
		slog.Debug("no focus tool found (xdotool / kdotool), cannot focus window", "module", "focus")
		return focusLinuxTryFailed
	}

	// Get the client TTY for this tmux session
	cmd := clcommon.TmuxCommand("list-clients", "-t", clcommon.ExactTarget(tmuxSession), "-F", "#{client_tty}")
	output, err := cmd.Output()
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to get tmux client tty: %v", err), "module", "focus")
		return focusLinuxTryFailed
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		slog.Debug("No clients attached to tmux session", "module", "focus")
		return focusLinuxNoClients
	}
	tty := lines[0]
	slog.Debug(fmt.Sprintf("Found client TTY: %s (tool=%s)", tty, tool), "module", "focus")

	// Find the terminal window by walking the process tree from TTY
	if focusLinuxWindowByTTY(tool, tty) {
		return focusLinuxFocused
	}
	// A client IS attached — activate failed. Do NOT fall back to
	// opening a new terminal; that would give the human two attached
	// clients to the same tmux session. Tell the caller so it logs
	// instead.
	return focusLinuxTryFailed
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

// =============================================================================
// Linux focus fallback — open a fresh terminal when no client is attached
// =============================================================================
//
// The TryFocusAttachedSessionWithID Linux path used to bail when tmux
// reported no attached clients ("No clients attached to tmux session"
// debug line), even though the dashboard's focus semantics promise
// "raise the window, OPENING a fresh one when none is open" — see
// pkg/claude/agentd/window_focus.go. WSL had focusWTTabByCycling for
// this; native Linux didn't. openLinuxAttachTerminal closes that gap.
//
// The new-window command mirrors agentd's openAttachCmd shape:
// absolute tclaude path (the terminal launches outside the user's
// login shell, where PATH may be minimal — same reason openShellCmd
// does it), single-quoted label, and an `exec ` prefix so the
// wrapping `sh -c` terminates by exec-replacement rather than normal
// exit. Both keep the tab close cleanly when the human later "hides"
// the agent (which detaches the tmux client → tclaude exits → tab
// closes, no shell-prompt limbo).

// linuxOpenTerminal is the terminal-launch seam for tests. Production
// points at terminal.OpenWithCommand; tests swap in a recorder.
var linuxOpenTerminal = terminal.OpenWithCommand

// openLinuxAttachTerminal opens a new terminal window that runs
// `tclaude session attach <sessionID>`. Best-effort: logs on failure
// but never errors — same contract as the rest of the focus path,
// which the dashboard treats as best-effort.
func openLinuxAttachTerminal(sessionID string) {
	if sessionID == "" {
		return
	}
	cmd := buildLinuxAttachCmd(sessionID)
	slog.Debug(fmt.Sprintf("Opening new terminal attaching to session %s", sessionID), "module", "focus")
	if err := linuxOpenTerminal(cmd); err != nil {
		slog.Debug(fmt.Sprintf("Failed to open attach terminal for %s: %v", sessionID, err), "module", "focus")
	}
}

// buildLinuxAttachCmd assembles the shell payload openLinuxAttachTerminal
// hands to the resolved terminal. Pure so the quoting + exec-prefix
// behaviour is unit-testable without spawning a real terminal.
func buildLinuxAttachCmd(sessionID string) string {
	return "exec " + clcommon.DetectAbsoluteCmd("session", "attach") + " " +
		linuxShellSingleQuote(sessionID)
}

// linuxShellSingleQuote wraps s as a single POSIX shell word. Labels
// come from the human-set agent title path and can carry spaces or
// quotes; this is the same belt-and-braces quoting agentd's
// openAttachCmd uses via its shellSingleQuote (kept local so the
// focus_linux.go package doesn't take a new dependency on agentd's
// unexported helpers).
func linuxShellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
