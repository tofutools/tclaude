package launcher

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// App launcher: a desktop entry (Linux) or an app bundle (macOS) that opens
// the dashboard, starting agentd first when it is not running. It is only
// launcher scripts and metadata; nothing is compiled or signed.
//
// Every file carries ManagedMarker (the .icns, which cannot, sits inside the
// bundle whose Info.plist does). tclaude rewrites or removes only those; a
// hand-made launcher at the same path is reported and left alone.

// Launcher icons, generated once by scripts/icons/make-launcher-icons.sh
// from the dashboard icon.
var (
	//go:embed assets/tclaude.svg
	launcherIconSVG []byte
	//go:embed assets/tclaude.icns
	launcherIconICNS []byte
)

// App launcher constants.
const (
	AppBundleID         = "dev.tofutools.tclaude.launcher"
	appBundleExecutable = "tclaude-launcher"
	// Kinds of app launcher.
	AppLauncherDesktopEntry = "desktop-entry"
	AppLauncherAppBundle    = "app-bundle"
	lsregisterPath          = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"
)

// AppLauncherOptions are the launcher's choices.
type AppLauncherOptions struct {
	// StartDaemon starts agentd (through the managed autostart unit when one
	// is installed, else detached) when it does not answer.
	StartDaemon bool `json:"start_daemon"`
}

// AppLauncherSpec is what to install.
type AppLauncherSpec struct {
	// Exe is the absolute path of the tclaude binary the launcher runs.
	Exe string
	// Version is the tclaude version, the bundle's version on macOS.
	Version string
	// PATH is exported by the launcher script, so a daemon it starts from
	// a desktop session (macOS gives apps a bare PATH) finds the harnesses.
	PATH    string
	Options AppLauncherOptions
}

// AppLauncherStatus is GET /api/node/launcher's launcher part.
type AppLauncherStatus struct {
	Supported bool `json:"supported"`
	// Kind is desktop-entry (Linux) or app-bundle (macOS).
	Kind string `json:"kind,omitempty"`
	// Path is the .desktop file or the .app bundle; Files every file the
	// launcher consists of.
	Path  string   `json:"path,omitempty"`
	Files []string `json:"files,omitempty"`
	// Installed: the launcher file exists; Managed: tclaude wrote it.
	Installed bool `json:"installed"`
	Managed   bool `json:"managed"`
	// Exe is the tclaude binary the installed launcher runs; Version the
	// tclaude version written into the bundle (macOS).
	Exe     string `json:"exe,omitempty"`
	Version string `json:"version,omitempty"`
	// CurrentExe and CurrentVersion are what an install now would write.
	CurrentExe     string `json:"current_exe,omitempty"`
	CurrentVersion string `json:"current_version,omitempty"`
	// Stale: a managed launcher that runs another binary (or, on macOS,
	// names another version) than this tclaude; agentd rewrites it at start.
	Stale       bool               `json:"stale"`
	StaleReason string             `json:"stale_reason,omitempty"`
	Options     AppLauncherOptions `json:"options"`
	Warnings    []string           `json:"warnings"`
}

// AppLauncherPaths is where the launcher lives.
type AppLauncherPaths struct {
	// Linux.
	Desktop string
	Icon    string
	Script  string
	// macOS.
	Bundle string
}

// AppLauncherPathsFor returns the launcher paths for goos under home,
// honouring XDG_DATA_HOME on Linux.
func AppLauncherPathsFor(goos, home, xdgData string) AppLauncherPaths {
	if goos == "darwin" {
		return AppLauncherPaths{Bundle: filepath.Join(home, "Applications", "tclaude.app")}
	}
	if xdgData == "" || !filepath.IsAbs(xdgData) {
		xdgData = filepath.Join(home, ".local", "share")
	}
	return AppLauncherPaths{
		Desktop: filepath.Join(xdgData, "applications", "tclaude.desktop"),
		Icon:    filepath.Join(xdgData, "icons", "hicolor", "scalable", "apps", "tclaude.svg"),
		Script:  filepath.Join(xdgData, "tclaude", "tclaude-launcher"),
	}
}

func (p AppLauncherPaths) plist() string { return filepath.Join(p.Bundle, "Contents", "Info.plist") }
func (p AppLauncherPaths) bundleScript() string {
	return filepath.Join(p.Bundle, "Contents", "MacOS", appBundleExecutable)
}
func (p AppLauncherPaths) icns() string {
	return filepath.Join(p.Bundle, "Contents", "Resources", "tclaude.icns")
}

// AppLauncher installs, removes and reports the app launcher. FS and Run are
// injected; tests never write the real HOME or run lsregister.
type AppLauncher struct {
	GOOS  string
	Paths AppLauncherPaths
	Run   Runner
	FS    FS
	// Current is what an install now writes (Exe, Version, PATH); Status
	// compares the installed launcher with it.
	Current AppLauncherSpec
}

// ErrLauncherNotManaged refuses to overwrite or remove a launcher tclaude
// did not write.
var ErrLauncherNotManaged = errors.New("a launcher tclaude did not write is installed there; remove or rename it yourself")

// ErrLauncherUnsupported: the app launcher exists for Linux and macOS only.
var ErrLauncherUnsupported = errors.New("the app launcher is supported on Linux and macOS")

func (l AppLauncher) supported() bool { return l.GOOS == "linux" || l.GOOS == "darwin" }

// mainFile is the file whose marker decides whether the launcher is ours.
func (l AppLauncher) mainFile() string {
	if l.GOOS == "darwin" {
		return l.Paths.plist()
	}
	return l.Paths.Desktop
}

// files is every file the launcher consists of, the main file first.
func (l AppLauncher) files() []string {
	if l.GOOS == "darwin" {
		return []string{l.Paths.plist(), l.Paths.bundleScript(), l.Paths.icns()}
	}
	return []string{l.Paths.Desktop, l.Paths.Script, l.Paths.Icon}
}

// markedFiles are the files that carry the marker themselves.
func (l AppLauncher) markedFiles() []string {
	if l.GOOS == "darwin" {
		return []string{l.Paths.plist(), l.Paths.bundleScript()}
	}
	return l.files()
}

// Status reads the installed launcher.
func (l AppLauncher) Status() AppLauncherStatus {
	st := AppLauncherStatus{Supported: l.supported(), Warnings: []string{}}
	if !st.Supported {
		return st
	}
	st.Kind, st.Path, st.Files = AppLauncherDesktopEntry, l.Paths.Desktop, l.files()
	if l.GOOS == "darwin" {
		st.Kind, st.Path = AppLauncherAppBundle, l.Paths.Bundle
	}
	st.CurrentExe, st.CurrentVersion = l.Current.Exe, l.Current.Version
	main, err := l.FS.ReadFile(l.mainFile())
	if err != nil {
		return st
	}
	st.Installed = true
	st.Managed = bytes.Contains(main, []byte(ManagedMarker))
	if !st.Managed {
		st.Warnings = append(st.Warnings, st.Path+" was not written by tclaude; tclaude leaves it alone.")
		return st
	}
	script, err := l.FS.ReadFile(l.scriptPath())
	if err != nil {
		st.Stale, st.StaleReason = true, "the launcher script is missing"
		return st
	}
	st.Exe, st.Options = parseLauncherScript(script)
	if l.GOOS == "darwin" {
		st.Version = plistString(main, "TclaudeVersion")
	}
	switch {
	case st.Exe == "":
		st.Stale, st.StaleReason = true, "the launcher script names no tclaude binary"
	case l.Current.Exe != "" && st.Exe != l.Current.Exe:
		st.Stale, st.StaleReason = true, "the launcher runs "+st.Exe+", not "+l.Current.Exe
	case l.GOOS == "darwin" && l.Current.Version != "" && st.Version != l.Current.Version:
		st.Stale, st.StaleReason = true, "the bundle names version "+st.Version+", not "+l.Current.Version
	}
	return st
}

func (l AppLauncher) scriptPath() string {
	if l.GOOS == "darwin" {
		return l.Paths.bundleScript()
	}
	return l.Paths.Script
}

// Install writes (or rewrites) the managed launcher, then asks the desktop
// to notice it (best effort).
func (l AppLauncher) Install(ctx context.Context, spec AppLauncherSpec) error {
	if !l.supported() {
		return ErrLauncherUnsupported
	}
	if err := ValidateAppLauncherSpec(spec); err != nil {
		return err
	}
	for _, f := range l.markedFiles() {
		if data, err := l.FS.ReadFile(f); err == nil && !bytes.Contains(data, []byte(ManagedMarker)) {
			return fmt.Errorf("%w: %s", ErrLauncherNotManaged, f)
		}
	}
	script := RenderLauncherScript(spec)
	if l.GOOS == "darwin" {
		for _, w := range []struct {
			path string
			data []byte
			mode os.FileMode
		}{
			{l.Paths.icns(), launcherIconICNS, 0o644},
			{l.Paths.bundleScript(), script, 0o755},
			// Info.plist last: it is what marks the bundle as ours.
			{l.Paths.plist(), RenderAppBundlePlist(spec), 0o644},
		} {
			if err := l.FS.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
				return err
			}
			if err := l.FS.WriteFile(w.path, w.data, w.mode); err != nil {
				return err
			}
		}
		// Launch Services and Spotlight would find it in ~/Applications
		// eventually; registering makes it searchable at once.
		l.bestEffort(ctx, lsregisterPath, "-f", l.Paths.Bundle)
		return nil
	}
	for _, w := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{l.Paths.Icon, markedSVG(), 0o644},
		{l.Paths.Script, script, 0o755},
		{l.Paths.Desktop, RenderDesktopEntry(spec, l.Paths), 0o644},
	} {
		if err := l.FS.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
			return err
		}
		if err := l.FS.WriteFile(w.path, w.data, w.mode); err != nil {
			return err
		}
	}
	l.bestEffort(ctx, "update-desktop-database", filepath.Dir(l.Paths.Desktop))
	return nil
}

// Uninstall removes the managed launcher. Missing files are fine; a
// launcher tclaude did not write is refused.
func (l AppLauncher) Uninstall(ctx context.Context) error {
	if !l.supported() {
		return ErrLauncherUnsupported
	}
	for _, f := range l.markedFiles() {
		if data, err := l.FS.ReadFile(f); err == nil && !bytes.Contains(data, []byte(ManagedMarker)) {
			return fmt.Errorf("%w: %s", ErrLauncherNotManaged, f)
		}
	}
	if l.GOOS == "darwin" {
		if _, err := l.FS.ReadFile(l.Paths.plist()); err == nil {
			// Unregister while the bundle is still there to name.
			l.bestEffort(ctx, lsregisterPath, "-u", l.Paths.Bundle)
		}
	}
	for _, f := range l.files() {
		if err := l.FS.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if l.GOOS == "darwin" {
		// The bundle's directories, innermost first; anything the operator
		// added keeps its directory (Remove fails on a non-empty one).
		c := filepath.Join(l.Paths.Bundle, "Contents")
		for _, d := range []string{filepath.Join(c, "MacOS"), filepath.Join(c, "Resources"), c, l.Paths.Bundle} {
			_ = l.FS.Remove(d)
		}
		return nil
	}
	_ = l.FS.Remove(filepath.Dir(l.Paths.Script))
	l.bestEffort(ctx, "update-desktop-database", filepath.Dir(l.Paths.Desktop))
	return nil
}

// bestEffort runs a desktop-integration command whose absence or failure
// does not matter: the launcher works without it, only less promptly.
func (l AppLauncher) bestEffort(ctx context.Context, name string, args ...string) {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	_, _ = l.Run(cctx, name, args...)
}

// launcherUnsafe are characters ValidateAppLauncherSpec refuses in paths,
// so a path needs no escaping in a desktop entry's Exec, a shell string or
// a plist.
const launcherUnsafe = "\"'`$\\%"

var bundleVersionRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}`)

// ValidateAppLauncherSpec rejects a relative binary and text that would need
// quoting in a desktop entry, a shell script or a plist.
func ValidateAppLauncherSpec(spec AppLauncherSpec) error {
	if !filepath.IsAbs(spec.Exe) {
		return fmt.Errorf("the tclaude binary must be an absolute path")
	}
	if err := plainText(spec.Exe, 4096); err != nil || strings.ContainsAny(spec.Exe, launcherUnsafe) {
		return fmt.Errorf("the tclaude binary path %q holds characters the launcher cannot carry", spec.Exe)
	}
	if err := plainText(spec.PATH, 32*1024); err != nil || strings.ContainsAny(spec.PATH, launcherUnsafe) {
		return fmt.Errorf("PATH holds characters the launcher cannot carry")
	}
	if err := plainText(spec.Version, 256); err != nil {
		return fmt.Errorf("version: %w", err)
	}
	return nil
}

// RenderLauncherScript renders the /bin/sh script the launcher runs. All the
// work is in `tclaude service launcher open`; the script only fixes the
// binary and PATH. ValidateAppLauncherSpec keeps both free of quotes.
func RenderLauncherScript(spec AppLauncherSpec) []byte {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# " + ManagedMarker + ": written by `tclaude service launcher install`.\n")
	b.WriteString("# tclaude rewrites or removes this file; to take it over, delete this line.\n")
	fmt.Fprintf(&b, "# tclaude-exe: %s\n", spec.Exe)
	fmt.Fprintf(&b, "# start-daemon: %t\n", spec.Options.StartDaemon)
	if spec.PATH != "" {
		fmt.Fprintf(&b, "PATH='%s'\nexport PATH\n", spec.PATH)
	}
	args := "service launcher open"
	if spec.Options.StartDaemon {
		args += " --start-daemon"
	}
	fmt.Fprintf(&b, "exec '%s' %s \"$@\"\n", spec.Exe, args)
	return []byte(b.String())
}

// parseLauncherScript reads back the binary and options a script names.
func parseLauncherScript(data []byte) (string, AppLauncherOptions) {
	var exe string
	var opts AppLauncherOptions
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "# tclaude-exe: "); ok {
			exe = v
		}
		if v, ok := strings.CutPrefix(line, "# start-daemon: "); ok {
			opts.StartDaemon = v == "true"
		}
	}
	return exe, opts
}

// RenderDesktopEntry renders the freedesktop.org desktop entry. StartupNotify
// is off: the launcher opens a browser tab, not a window of its own, so a
// startup notification would spin until it timed out.
func RenderDesktopEntry(spec AppLauncherSpec, p AppLauncherPaths) []byte {
	var b strings.Builder
	b.WriteString("# " + ManagedMarker + ": written by `tclaude service launcher install`.\n")
	b.WriteString("# tclaude rewrites or removes this file; to take it over, delete this line.\n")
	b.WriteString("[Desktop Entry]\nType=Application\nVersion=1.0\nName=tclaude\n")
	b.WriteString("GenericName=Agent dashboard\nComment=Open the tclaude agent dashboard\n")
	fmt.Fprintf(&b, "Exec=\"%s\"\n", p.Script)
	fmt.Fprintf(&b, "Icon=%s\n", p.Icon)
	b.WriteString("Terminal=false\nCategories=Development;\nKeywords=agents;dashboard;claude;codex;\nStartupNotify=false\n")
	return []byte(b.String())
}

// RenderAppBundlePlist renders the bundle's Info.plist.
//
// The executable is a shell script, which carries no architecture, and
// Launch Services on Apple Silicon has been seen to take such a bundle for
// an Intel one and offer to install Rosetta. LSArchitecturePriority listing
// arm64 first is the reported fix, LSRequiresNativeExecution forbids
// translation outright, and the bundle identifier is one no earlier build
// used, so no cached "Intel" verdict applies to it.
func RenderAppBundlePlist(spec AppLauncherSpec) []byte {
	short := bundleVersionRe.FindString(strings.TrimPrefix(spec.Version, "v"))
	if short == "" {
		short = "0.0.0"
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<!-- " + ManagedMarker + ": written by `tclaude service launcher install`. tclaude rewrites or removes this bundle; to take it over, delete this comment. -->\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	str := func(k, v string) { fmt.Fprintf(&b, "\t<key>%s</key>\n\t<string>%s</string>\n", k, xmlText(v)) }
	flag := func(k string) { fmt.Fprintf(&b, "\t<key>%s</key>\n\t<true/>\n", k) }
	str("CFBundleIdentifier", AppBundleID)
	str("CFBundleName", "tclaude")
	str("CFBundleDisplayName", "tclaude")
	str("CFBundleExecutable", appBundleExecutable)
	str("CFBundleIconFile", "tclaude.icns")
	str("CFBundlePackageType", "APPL")
	str("CFBundleInfoDictionaryVersion", "6.0")
	str("CFBundleShortVersionString", short)
	str("CFBundleVersion", short)
	str("TclaudeVersion", spec.Version)
	str("LSMinimumSystemVersion", "11.0")
	str("LSApplicationCategoryType", "public.app-category.developer-tools")
	b.WriteString("\t<key>LSArchitecturePriority</key>\n\t<array>\n\t\t<string>arm64</string>\n\t\t<string>x86_64</string>\n\t</array>\n")
	flag("LSRequiresNativeExecution")
	flag("NSHighResolutionCapable")
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

// plistString reads a top-level string value from a plist tclaude wrote.
func plistString(data []byte, key string) string {
	_, rest, ok := strings.Cut(string(data), "<key>"+key+"</key>")
	if !ok {
		return ""
	}
	_, rest, ok = strings.Cut(rest, "<string>")
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, "</string>")
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&#34;", `"`, "&#39;", "'", "&#x9;", "\t", "&#xA;", "\n", "&#xD;", "\r", "&amp;", "&").Replace(v)
}

// markedSVG is the icon with the marker as an XML comment.
func markedSVG() []byte {
	return append([]byte("<!-- "+ManagedMarker+": installed by `tclaude service launcher install`. -->\n"), launcherIconSVG...)
}
