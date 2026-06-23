//go:build linux

package dirpicker

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
)

// pick drives a native folder chooser on Linux, preferring zenity (GTK)
// and falling back to kdialog (KDE). When neither is installed it returns
// ErrUnavailable so the caller can ask the human to type the path.
func pick(ctx context.Context, opts Options) (string, error) {
	title := opts.Title
	if title == "" {
		title = "Select a directory"
	}
	if bin, err := exec.LookPath("zenity"); err == nil {
		args := []string{"--file-selection", "--directory", "--title=" + title}
		if opts.StartDir != "" {
			// zenity opens *inside* the start dir only when the filename
			// hint ends in a separator.
			args = append(args, "--filename="+strings.TrimRight(opts.StartDir, "/")+"/")
		}
		return runLinuxPicker(ctx, bin, args)
	}
	if bin, err := exec.LookPath("kdialog"); err == nil {
		start := opts.StartDir
		if start == "" {
			start = "."
		}
		return runLinuxPicker(ctx, bin, []string{"--getexistingdirectory", start, "--title", title})
	}
	return "", ErrUnavailable
}

// runLinuxPicker runs a picker binary and maps its exit convention onto
// our contract: stdout is the chosen path; a non-zero exit with no path
// is the human cancelling (both zenity and kdialog exit 1 on cancel).
// stderr is discarded — zenity is chatty with GTK warnings even on
// success, so it makes a poor cancel/error signal.
func runLinuxPicker(ctx context.Context, bin string, args []string) (string, error) {
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = &stdout
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	if err != nil && out == "" {
		return "", ErrCanceled
	}
	return out, nil
}
