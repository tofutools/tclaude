//go:build windows

package dirpicker

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// pick drives the classic Win32 folder browser via PowerShell's
// System.Windows.Forms.FolderBrowserDialog. The dialog needs a
// single-threaded apartment, hence -STA.
//
// The script distinguishes the two failure modes via its exit code so a
// genuine environment failure isn't silently reported as a cancel: a
// non-OK dialog result exits 1 (→ ErrCanceled), while an exception (no
// WinForms assembly, no desktop session, …) is caught and exits 2 with
// the message on stderr (→ a real error).
//
// NOTE: this backend is forward-looking — the only importer, the agentd
// daemon, is currently POSIX-only, so on Windows nothing calls Pick yet.
// It's kept so the package is whole and ready if agentd ever ships there.
func pick(ctx context.Context, opts Options) (string, error) {
	ps := powershellBinary()
	if ps == "" {
		// No PowerShell on PATH → no usable picker; let the caller fall
		// back to asking the human to type the path (mirrors Linux's
		// zenity/kdialog LookPath → ErrUnavailable).
		return "", ErrUnavailable
	}
	title := opts.Title
	if title == "" {
		title = "Select a directory"
	}
	var b strings.Builder
	b.WriteString("try {")
	b.WriteString("Add-Type -AssemblyName System.Windows.Forms;")
	b.WriteString("$d = New-Object System.Windows.Forms.FolderBrowserDialog;")
	b.WriteString("$d.Description = '")
	b.WriteString(escapePowerShellSingleQuote(title))
	b.WriteString("';")
	b.WriteString("$d.ShowNewFolderButton = $true;")
	if opts.StartDir != "" {
		b.WriteString("$d.SelectedPath = '")
		b.WriteString(escapePowerShellSingleQuote(opts.StartDir))
		b.WriteString("';")
	}
	b.WriteString("if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::Out.Write($d.SelectedPath) } else { exit 1 }")
	b.WriteString("} catch { [Console]::Error.Write($_.Exception.Message); exit 2 }")

	out, err := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-STA", "-Command", b.String()).Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err() // client disconnected; dialog torn down
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if ee.ExitCode() == 1 {
				return "", ErrCanceled // user dismissed the dialog
			}
			if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
				return "", fmt.Errorf("powershell: %s", msg)
			}
			return "", ErrCanceled
		}
		return "", err
	}
	return string(out), nil
}

// powershellBinary returns the first PowerShell on PATH — Windows
// PowerShell (powershell.exe, ships with the OS) preferred, then
// PowerShell 7+ (pwsh) — or "" when neither is found.
func powershellBinary() string {
	for _, name := range []string{"powershell", "pwsh"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// escapePowerShellSingleQuote escapes s for a PowerShell single-quoted
// string literal, where the only metacharacter is the quote itself,
// escaped by doubling.
func escapePowerShellSingleQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
