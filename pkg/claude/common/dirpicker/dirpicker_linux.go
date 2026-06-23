//go:build linux

package dirpicker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
// our contract, distinguishing the three outcomes the caller cares about:
//
//   - clean exit (0) — stdout is the chosen path;
//   - the caller's context ended (client disconnected) — surface ctx.Err();
//   - exit 1 — the human dismissed the dialog (both zenity and kdialog
//     exit 1 on cancel) → ErrCanceled;
//   - anything else (no display, GTK init failure, the binary blew up) —
//     a genuine error carrying the tool's stderr, NOT a phantom cancel.
//
// stderr is captured but only read on the genuine-error path, so zenity's
// GTK chatter on a normal run or a cancel never leaks into a result.
func runLinuxPicker(ctx context.Context, bin string, args []string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	if err == nil {
		return out, nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	// A path despite a non-zero exit — trust it.
	if out != "" {
		return out, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return "", ErrCanceled
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" {
		return "", fmt.Errorf("%s: %s", bin, msg)
	}
	return "", err
}
