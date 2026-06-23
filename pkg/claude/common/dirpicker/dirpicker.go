// Package dirpicker opens a native OS directory-selection dialog and
// reports the chosen path. It is the desktop-side bridge the browser
// dashboard cannot reach on its own: a web page has no way to pop a
// native folder picker, but agentd — running as the human, on the
// human's desktop, outside any agent sandbox — can, and then hands the
// chosen path back over the loopback API.
//
// Pick is implemented per platform (dirpicker_{darwin,linux,windows}.go):
// osascript `choose folder` on macOS, zenity/kdialog on Linux, a
// PowerShell FolderBrowserDialog on Windows. This file holds the
// platform-agnostic shell: the request options, the sentinel errors, and
// the result normalisation shared by every backend.
package dirpicker

import (
	"context"
	"errors"
	"os"
	"strings"
)

// Options configures a directory-picker dialog.
type Options struct {
	// Title is the dialog's prompt / heading. A sensible default is used
	// when empty.
	Title string
	// StartDir is the directory the dialog initially shows. It is
	// ignored when empty or when it does not point at an existing
	// directory, so a stale or half-typed path never makes the picker
	// fail — it just opens at the platform default.
	StartDir string
}

// Sentinel errors callers match with errors.Is.
var (
	// ErrCanceled is returned when the human dismisses the dialog
	// without choosing a directory. It is an expected outcome, not a
	// failure — callers should treat it as "no change".
	ErrCanceled = errors.New("dirpicker: selection canceled")
	// ErrUnavailable is returned when the platform has no usable picker
	// (e.g. Linux without zenity or kdialog installed). Callers should
	// fall back to asking the human to type the path.
	ErrUnavailable = errors.New("dirpicker: no native directory picker available")
)

// Pick opens a native directory-selection dialog and blocks until the
// human chooses a directory or cancels. It returns the chosen path
// (no trailing separator), ErrCanceled if the dialog was dismissed, or
// ErrUnavailable when no picker exists on this machine. Cancelling ctx
// (e.g. the HTTP client disconnecting) tears the dialog down.
func Pick(ctx context.Context, opts Options) (string, error) {
	opts.StartDir = sanitizeStartDir(opts.StartDir)
	raw, err := pick(ctx, opts)
	if err != nil {
		return "", err
	}
	path := normalizeChosenPath(raw)
	if path == "" {
		// A backend that exits cleanly but yields nothing is treated as
		// a cancel rather than handing the caller an empty path it would
		// have to special-case.
		return "", ErrCanceled
	}
	return path, nil
}

// sanitizeStartDir returns dir when it is an existing directory, else "".
func sanitizeStartDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ""
	}
	return dir
}

// normalizeChosenPath trims the trailing newline a CLI picker prints and
// the trailing separator some backends append (macOS `choose folder`
// yields "/Users/me/dir/"), while preserving a bare root ("/").
func normalizeChosenPath(raw string) string {
	path := strings.TrimSpace(raw)
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return path
}
