package launcher

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

var launcherSpec = AppLauncherSpec{
	Exe: "/home/u/.local/bin/tclaude", Version: "v1.42.0", PATH: "/home/u/.local/bin:/usr/bin",
	Options: AppLauncherOptions{StartDaemon: true},
}

func newTestAppLauncher(goos string) (AppLauncher, memFS, *fakeRun) {
	fs, run := memFS{}, &fakeRun{}
	return AppLauncher{GOOS: goos, Paths: AppLauncherPathsFor(goos, "/home/u", ""), Run: run.run, FS: fs, Current: launcherSpec}, fs, run
}

func TestAppLauncherLinuxRoundTrip(t *testing.T) {
	l, fs, run := newTestAppLauncher("linux")
	if l.Paths.Desktop != "/home/u/.local/share/applications/tclaude.desktop" ||
		l.Paths.Icon != "/home/u/.local/share/icons/hicolor/scalable/apps/tclaude.svg" {
		t.Fatalf("paths: %+v", l.Paths)
	}
	if p := AppLauncherPathsFor("linux", "/home/u", "/xdg"); p.Desktop != "/xdg/applications/tclaude.desktop" {
		t.Fatalf("XDG_DATA_HOME not honoured: %+v", p)
	}
	if st := l.Status(); !st.Supported || st.Installed || st.Kind != AppLauncherDesktopEntry {
		t.Fatalf("before install: %+v", st)
	}
	if err := l.Install(context.Background(), launcherSpec); err != nil {
		t.Fatal(err)
	}
	desktop := string(fs[l.Paths.Desktop])
	for _, want := range []string{ManagedMarker, "[Desktop Entry]", "Type=Application", "Name=tclaude", "Comment=",
		`Exec="/home/u/.local/share/tclaude/tclaude-launcher"`, "Icon=" + l.Paths.Icon, "Categories=Development;",
		"Terminal=false", "StartupNotify=false"} {
		if !strings.Contains(desktop, want) {
			t.Errorf(".desktop lacks %q:\n%s", want, desktop)
		}
	}
	script := string(fs[l.Paths.Script])
	for _, want := range []string{"#!/bin/sh\n", ManagedMarker, "PATH='/home/u/.local/bin:/usr/bin'\nexport PATH",
		`exec '/home/u/.local/bin/tclaude' service launcher open --start-daemon "$@"`} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if !bytes.Contains(fs[l.Paths.Icon], []byte(ManagedMarker)) || !bytes.Contains(fs[l.Paths.Icon], []byte("<svg")) {
		t.Errorf("icon: %s", fs[l.Paths.Icon])
	}
	if !strings.Contains(strings.Join(run.calls, "\n"), "update-desktop-database /home/u/.local/share/applications") {
		t.Errorf("calls: %v", run.calls)
	}
	st := l.Status()
	if !st.Installed || !st.Managed || st.Stale || st.Exe != launcherSpec.Exe || !st.Options.StartDaemon || len(st.Files) != 3 {
		t.Fatalf("after install: %+v", st)
	}

	// A moved binary makes it stale; an install without start_daemon drops the flag.
	l.Current.Exe = "/opt/tclaude/bin/tclaude"
	if st := l.Status(); !st.Stale || !strings.Contains(st.StaleReason, "/opt/tclaude/bin/tclaude") {
		t.Fatalf("stale: %+v", st)
	}
	spec := l.Current
	spec.Options.StartDaemon = false
	if err := l.Install(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if st := l.Status(); st.Stale || st.Options.StartDaemon || strings.Contains(string(fs[l.Paths.Script]), "--start-daemon") {
		t.Fatalf("reinstall: %+v\n%s", st, fs[l.Paths.Script])
	}

	if err := l.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fs) != 0 {
		t.Fatalf("left behind: %v", fs)
	}
}

func TestAppLauncherMacOSBundle(t *testing.T) {
	l, fs, run := newTestAppLauncher("darwin")
	if l.Paths.Bundle != "/home/u/Applications/tclaude.app" {
		t.Fatalf("bundle: %s", l.Paths.Bundle)
	}
	if err := l.Install(context.Background(), launcherSpec); err != nil {
		t.Fatal(err)
	}
	plist := string(fs["/home/u/Applications/tclaude.app/Contents/Info.plist"])
	for _, want := range []string{ManagedMarker,
		"<key>CFBundleIdentifier</key>\n\t<string>dev.tofutools.tclaude.launcher</string>",
		"<key>CFBundleExecutable</key>\n\t<string>tclaude-launcher</string>",
		"<key>CFBundlePackageType</key>\n\t<string>APPL</string>",
		"<key>CFBundleShortVersionString</key>\n\t<string>1.42.0</string>",
		"<key>CFBundleIconFile</key>\n\t<string>tclaude.icns</string>",
		"<key>LSArchitecturePriority</key>\n\t<array>\n\t\t<string>arm64</string>\n\t\t<string>x86_64</string>\n\t</array>",
		"<key>LSRequiresNativeExecution</key>\n\t<true/>",
		"<key>NSHighResolutionCapable</key>\n\t<true/>",
		"<key>LSMinimumSystemVersion</key>"} {
		if !strings.Contains(plist, want) {
			t.Errorf("Info.plist lacks %q:\n%s", want, plist)
		}
	}
	if !bytes.HasPrefix(fs["/home/u/Applications/tclaude.app/Contents/Resources/tclaude.icns"], []byte("icns")) {
		t.Error("no .icns")
	}
	if !strings.HasPrefix(string(fs["/home/u/Applications/tclaude.app/Contents/MacOS/tclaude-launcher"]), "#!/bin/sh\n") {
		t.Error("no launcher script")
	}
	if !strings.Contains(strings.Join(run.calls, "\n"), "lsregister -f /home/u/Applications/tclaude.app") {
		t.Errorf("calls: %v", run.calls)
	}
	if st := l.Status(); !st.Managed || st.Stale || st.Version != "v1.42.0" || st.Kind != AppLauncherAppBundle {
		t.Fatalf("status: %+v", st)
	}
	// An update changes the version the bundle names.
	l.Current.Version = "v1.43.0"
	if st := l.Status(); !st.Stale {
		t.Fatalf("version change not stale: %+v", st)
	}
	if err := l.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fs) != 0 {
		t.Fatalf("left behind: %v", fs)
	}
	if !strings.Contains(strings.Join(run.calls, "\n"), "lsregister -u /home/u/Applications/tclaude.app") {
		t.Errorf("calls: %v", run.calls)
	}
}

func TestAppLauncherLeavesHandMadeLaunchersAlone(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		l, fs, _ := newTestAppLauncher(goos)
		mine := []byte("[Desktop Entry]\nExec=/my/own\n")
		fs[l.mainFile()] = mine
		st := l.Status()
		if !st.Installed || st.Managed || len(st.Warnings) == 0 {
			t.Fatalf("%s status: %+v", goos, st)
		}
		if err := l.Install(context.Background(), launcherSpec); !errors.Is(err, ErrLauncherNotManaged) {
			t.Fatalf("%s install: %v", goos, err)
		}
		if err := l.Uninstall(context.Background()); !errors.Is(err, ErrLauncherNotManaged) {
			t.Fatalf("%s uninstall: %v", goos, err)
		}
		if !bytes.Equal(fs[l.mainFile()], mine) || len(fs) != 1 {
			t.Fatalf("%s: hand-made launcher touched: %v", goos, fs)
		}
	}
	// A hand-made script next to a managed entry is refused too.
	l, fs, _ := newTestAppLauncher("linux")
	fs[l.Paths.Script] = []byte("#!/bin/sh\nexec mine\n")
	if err := l.Install(context.Background(), launcherSpec); !errors.Is(err, ErrLauncherNotManaged) {
		t.Fatalf("install over a hand-made script: %v", err)
	}
}

func TestAppLauncherRejectsUnsafeText(t *testing.T) {
	for _, spec := range []AppLauncherSpec{
		{Exe: "tclaude"},
		{Exe: "/bin/tc'laude"},
		{Exe: "/bin/tclaude\nexec evil"},
		{Exe: "/bin/$(evil)"},
		{Exe: "/bin/tclaude", PATH: "/usr/bin:'; evil; '"},
	} {
		if err := ValidateAppLauncherSpec(spec); err == nil {
			t.Errorf("accepted %+v", spec)
		}
	}
	l, _, _ := newTestAppLauncher("windows")
	if st := l.Status(); st.Supported {
		t.Fatal("windows supported")
	}
	if err := l.Install(context.Background(), launcherSpec); !errors.Is(err, ErrLauncherUnsupported) {
		t.Fatal(err)
	}
}

func TestBundleVersionFallsBackForDevBuilds(t *testing.T) {
	plist := string(RenderAppBundlePlist(AppLauncherSpec{Exe: "/x", Version: "(devel)"}))
	if !strings.Contains(plist, "<key>CFBundleShortVersionString</key>\n\t<string>0.0.0</string>") ||
		!strings.Contains(plist, "<key>TclaudeVersion</key>\n\t<string>(devel)</string>") {
		t.Fatal(plist)
	}
	if got := plistString([]byte(plist), "TclaudeVersion"); got != "(devel)" {
		t.Fatal(got)
	}
}
