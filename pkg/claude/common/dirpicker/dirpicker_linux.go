//go:build linux

package dirpicker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// positionTimeout bounds how long the best-effort positioner waits for the
// picker window to map before giving up and leaving it where the WM put it.
const positionTimeout = 4 * time.Second

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
		// GTK defaults to the Wayland backend when one is present, which
		// makes the dialog an unpositionable native surface; pin it to
		// XWayland so we can place it next to the pointer.
		return runLinuxPicker(ctx, bin, args, "GDK_BACKEND=x11")
	}
	if bin, err := exec.LookPath("kdialog"); err == nil {
		start := opts.StartDir
		if start == "" {
			start = "."
		}
		// Same idea for Qt/KDE: force the X11 (xcb) platform plugin.
		return runLinuxPicker(ctx, bin, []string{"--getexistingdirectory", start, "--title", title}, "QT_QPA_PLATFORM=xcb")
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
//
// x11Env, when non-empty (e.g. "GDK_BACKEND=x11"), pins the toolkit to its X11
// backend so the dialog is an addressable XWayland window — but only when an X
// server is actually reachable, since otherwise the override would break the
// picker on a pure-Wayland or headless box. With the override in effect we
// launch asynchronously and best-effort-nudge the window next to the pointer.
func runLinuxPicker(ctx context.Context, bin string, args []string, x11Env string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	position := x11Env != "" && x11Available()
	if position {
		cmd.Env = append(os.Environ(), x11Env)
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	if position {
		posCtx, cancel := context.WithTimeout(ctx, positionTimeout)
		defer cancel()
		//nolint:gosec // a pid fits a uint32; X11 _NET_WM_PID is a CARDINAL
		go positionPickerNearPointer(posCtx, uint32(cmd.Process.Pid))
	}
	err := cmd.Wait()
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
