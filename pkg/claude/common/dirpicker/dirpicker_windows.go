//go:build windows

package dirpicker

import (
	"context"
	"errors"
	"os/exec"
	"strings"
)

// pick drives the classic Win32 folder browser via PowerShell's
// System.Windows.Forms.FolderBrowserDialog. The dialog needs a
// single-threaded apartment, hence -STA. Cancelling makes the script
// exit 1, which we map to ErrCanceled.
func pick(ctx context.Context, opts Options) (string, error) {
	title := opts.Title
	if title == "" {
		title = "Select a directory"
	}
	var b strings.Builder
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

	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command", b.String()).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// Any non-zero exit from the script is the human cancelling
			// (we exit 1 ourselves on a non-OK dialog result).
			return "", ErrCanceled
		}
		return "", err
	}
	return string(out), nil
}

// escapePowerShellSingleQuote escapes s for a PowerShell single-quoted
// string literal, where the only metacharacter is the quote itself,
// escaped by doubling.
func escapePowerShellSingleQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
