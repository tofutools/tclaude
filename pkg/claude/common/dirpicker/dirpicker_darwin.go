//go:build darwin

package dirpicker

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// pick drives macOS's native folder chooser. The primary path is a JXA
// (JavaScript for Automation) script that drives an NSOpenPanel directly:
// it promotes THIS osascript process to a regular foreground app
// (setActivationPolicy + activateIgnoringOtherApps) and only then runs the
// panel, so the panel becomes the key window and takes keyboard focus.
//
// This is the crux of the focus problem. agentd spawns osascript while the
// browser is the active app. AppleScript's `choose folder` — and the same
// command run via System Events — shows a panel that appears on top but
// never becomes key: the daemon-spawned process can't make itself active
// through the old Process-Manager path (`tell me to activate` just stalls),
// and an accessory host like System Events deactivates the browser yet
// still can't hand its panel key focus, so keystrokes go nowhere. Driving
// NSOpenPanel from a process we explicitly activate via AppKit fixes that.
// The process briefly shows a Dock icon while the panel is up; it vanishes
// when osascript exits.
//
// On cancel the script prints nothing (exit 0); the shared Pick maps the
// empty result to ErrCanceled. If JXA itself fails (an ancient macOS, a
// broken bridge), pick falls back to a plain AppleScript `choose folder` —
// which works but may not be focused — so the picker is never worse than
// the no-focus version, rather than hard-failing.
func pick(ctx context.Context, opts Options) (string, error) {
	title := opts.Title
	if title == "" {
		title = "Select a directory"
	}
	out, err := runJXAPanel(ctx, title, opts.StartDir)
	if err == nil {
		return out, nil // "" on cancel → Pick maps to ErrCanceled
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	// JXA unavailable / errored — degrade to the plain AppleScript chooser.
	chooser := `choose folder with prompt "` + escapeAppleScript(title) + `"`
	if opts.StartDir != "" {
		chooser += ` default location (POSIX file "` + escapeAppleScript(opts.StartDir) + `")`
	}
	return runAppleScriptChooser(ctx, `POSIX path of (`+chooser+`)`)
}

// runJXAPanel shows an NSOpenPanel via JXA from an explicitly-activated
// app so the panel takes keyboard focus. It returns the chosen path, or ""
// when the human cancels (the script prints nothing and exits 0).
//
// The activation-policy / modal-response integers are written as literals
// to avoid depending on enum-constant resolution in the JXA bridge:
// NSApplicationActivationPolicyRegular == 0, NSModalResponseOK == 1.
func runJXAPanel(ctx context.Context, title, startDir string) (string, error) {
	var b strings.Builder
	b.WriteString("ObjC.import('AppKit');")
	b.WriteString("var app = $.NSApplication.sharedApplication;")
	b.WriteString("app.setActivationPolicy(0);")
	b.WriteString("app.activateIgnoringOtherApps(true);")
	b.WriteString("var p = $.NSOpenPanel.openPanel;")
	b.WriteString("p.canChooseFiles = false;")
	b.WriteString("p.canChooseDirectories = true;")
	b.WriteString("p.allowsMultipleSelection = false;")
	b.WriteString(`p.message = "`)
	b.WriteString(escapeJSString(title))
	b.WriteString(`";`)
	b.WriteString(`p.prompt = "Choose";`)
	if startDir != "" {
		b.WriteString(`p.directoryURL = $.NSURL.fileURLWithPath("`)
		b.WriteString(escapeJSString(startDir))
		b.WriteString(`");`)
	}
	b.WriteString(`(p.runModal == 1) ? ObjC.unwrap(p.URLs.objectAtIndex(0).path) : "";`)

	out, err := exec.CommandContext(ctx, "osascript", "-l", "JavaScript", "-e", b.String()).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("osascript(jxa): %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// runAppleScriptChooser runs an osascript `choose folder` program and maps
// its outcome onto our contract: stdout is the chosen path; a
// "User canceled. (-128)" failure is ErrCanceled; any other non-zero exit
// is an error carrying osascript's stderr.
func runAppleScriptChooser(ctx context.Context, script string) (string, error) {
	out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr := strings.ToLower(string(ee.Stderr))
			if strings.Contains(stderr, "-128") || strings.Contains(stderr, "user canceled") {
				return "", ErrCanceled
			}
			return "", fmt.Errorf("osascript: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// escapeAppleScript escapes s for interpolation inside an AppleScript
// double-quoted string literal.
func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}

// escapeJSString escapes s for interpolation inside a double-quoted
// JavaScript string literal (the JXA script passed to osascript).
func escapeJSString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	return s
}
