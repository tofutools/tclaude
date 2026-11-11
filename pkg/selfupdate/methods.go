package selfupdate

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Self-update method IDs. The operator picks one; detection only recommends.
const (
	MethodRelease        = "release"
	MethodGoInstall      = "go_install"
	MethodSourceCheckout = "source_checkout"
	MethodHomebrew       = "homebrew"
	MethodNixProfile     = "nix_profile"
	MethodMise           = "mise"
	MethodApt            = "apt"
	MethodDnf            = "dnf"
	MethodCustom         = "custom"
	MethodManual         = "manual"
)

// MethodIDs lists every method in the order a chooser shows them.
var MethodIDs = []string{MethodRelease, MethodGoInstall, MethodSourceCheckout, MethodHomebrew, MethodNixProfile, MethodMise, MethodApt, MethodDnf, MethodCustom, MethodManual}

// Package managers a binary can be owned by (Binary.ManagedBy).
const (
	ManagerHomebrew   = "homebrew"
	ManagerNixProfile = "nix_profile"
	ManagerNix        = "nix" // declarative: NixOS, home-manager, a flake
	ManagerMise       = "mise"
	ManagerAsdf       = "asdf"
	ManagerDpkg       = "dpkg"
	ManagerRpm        = "rpm"
)

// HomebrewFormula is the formula the homebrew method upgrades.
const HomebrewFormula = "tofutools/tap/tclaude"

// managerMethod maps a package manager to the method that drives it.
var managerMethod = map[string]string{ManagerHomebrew: MethodHomebrew, ManagerNixProfile: MethodNixProfile, ManagerMise: MethodMise, ManagerDpkg: MethodApt, ManagerRpm: MethodDnf}

// IsPackageManagerMethod reports whether running the method means running a
// package manager. Those are always an explicit opt-in.
func IsPackageManagerMethod(m string) bool {
	switch m {
	case MethodHomebrew, MethodNixProfile, MethodMise, MethodApt, MethodDnf:
		return true
	}
	return false
}

// IsFreeTextMethod reports whether the method's configuration is free
// operator text (a command or a path), which only the local operator or an
// unrestricted peer may set.
func IsFreeTextMethod(m string) bool { return m == MethodCustom || m == MethodSourceCheckout }

// TclaudeDriven methods write the binaries themselves (or run the operator's
// own command), as opposed to a package manager or nothing.
func TclaudeDriven(m string) bool {
	switch m {
	case MethodRelease, MethodGoInstall, MethodSourceCheckout, MethodCustom:
		return true
	}
	return false
}

// System is the host boundary detection and package-manager methods use.
type System struct {
	LookPath func(string) (string, error)
	// Run runs a fixed argv (never a shell) and returns bounded combined output.
	Run func(ctx context.Context, argv []string, env []string) ([]byte, error)
}

var systemMu sync.Mutex
var system = System{LookPath: exec.LookPath, Run: runFixed}

func currentSystem() System {
	systemMu.Lock()
	defer systemMu.Unlock()
	return system
}

// SetSystemForTest swaps the host boundary; it returns a restore function.
func SetSystemForTest(s System) func() {
	systemMu.Lock()
	prev := system
	system = s
	systemMu.Unlock()
	return func() {
		systemMu.Lock()
		system = prev
		systemMu.Unlock()
	}
}

func runFixed(ctx context.Context, argv []string, env []string) ([]byte, error) {
	name := argv[0]
	if env != nil {
		// Found where the command's environment would find it, not on
		// this process's PATH (exec.Command's lookup).
		if p, ok := lookPathIn(name, env); ok {
			name = p
		}
	}
	cmd := exec.CommandContext(ctx, name, argv[1:]...)
	if env != nil {
		cmd.Env = env
	}
	cmd.WaitDelay = time.Second
	var out tailBuffer
	out.limit = 64 << 10
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.data, err
}

// tailBuffer keeps the last limit bytes written.
type tailBuffer struct {
	data  []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		b.data = append([]byte{}, b.data[len(b.data)-b.limit:]...)
	}
	return len(p), nil
}

// tail returns at most n trailing bytes of s, trimmed.
func tail(s []byte, n int) string {
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return strings.TrimSpace(string(s))
}

var packageNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._:/@-]{0,127}$`)

// PackageManagerTree reports whether a path is inside a tree a package
// manager owns, which tclaude never writes into.
func PackageManagerTree(path string) bool {
	return path == "/usr" || strings.HasPrefix(path, "/usr/") || path == "/nix" || strings.HasPrefix(path, "/nix/") || strings.Contains(path, "/Cellar/") || strings.HasSuffix(path, "/Cellar") ||
		strings.Contains(path+"/", "/mise/installs/") || strings.Contains(path+"/", "/.asdf/installs/")
}

func probe(argv ...string) (string, bool) {
	sys := currentSystem()
	if _, err := sys.LookPath(argv[0]); err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := sys.Run(ctx, argv, nil)
	return strings.TrimSpace(string(out)), err == nil
}

// classify fills ManagedBy, Package and Evidence for a resolved binary path.
func classify(b *Binary) {
	path := b.Path
	switch {
	case strings.Contains(path, "/Cellar/tclaude/"):
		b.ManagedBy, b.Package, b.Evidence = ManagerHomebrew, HomebrewFormula, "resolves into a Homebrew Cellar: "+path
	case strings.HasPrefix(path, "/nix/store/"):
		b.ManagedBy, b.Evidence = ManagerNix, "lives in the Nix store: "+path
		if out, ok := probe("nix", "profile", "list", "--json"); ok {
			if name := nixProfileEntry(out, path); name != "" {
				b.ManagedBy, b.Package, b.Evidence = ManagerNixProfile, name, "installed in the imperative Nix profile as "+name
			}
		}
	case strings.Contains(path, "/mise/installs/"):
		b.ManagedBy, b.Evidence = ManagerMise, "lives in a mise install dir: "+path
		if out, ok := probe("mise", "ls", "--installed", "--json"); ok {
			b.Package = miseTool(out, path)
		}
	case strings.Contains(path, "/.asdf/installs/"):
		b.ManagedBy, b.Evidence = ManagerAsdf, "lives in an asdf install dir: "+path
	case strings.HasPrefix(path, "/usr/") || strings.HasPrefix(path, "/opt/"):
		if out, ok := probe("dpkg", "-S", path); ok {
			if name, _, found := strings.Cut(out, ":"); found && packageNamePattern.MatchString(strings.TrimSpace(name)) {
				b.ManagedBy, b.Package, b.Evidence = ManagerDpkg, strings.TrimSpace(name), "owned by the dpkg package "+strings.TrimSpace(name)
				return
			}
		}
		if out, ok := probe("rpm", "-qf", "--qf", "%{NAME}", path); ok && packageNamePattern.MatchString(out) {
			b.ManagedBy, b.Package, b.Evidence = ManagerRpm, out, "owned by the rpm package "+out
		}
	}
	if b.Package != "" && !packageNamePattern.MatchString(b.Package) {
		b.Package = ""
	}
}

func nixProfileEntry(raw, path string) string {
	var doc struct {
		Elements json.RawMessage `json:"elements"`
	}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return ""
	}
	type element struct {
		StorePaths []string `json:"storePaths"`
	}
	matches := func(e element) bool {
		for _, sp := range e.StorePaths {
			if sp != "" && (path == sp || strings.HasPrefix(path, sp+"/")) {
				return true
			}
		}
		return false
	}
	named := map[string]element{}
	if json.Unmarshal(doc.Elements, &named) == nil {
		for name, e := range named {
			if matches(e) {
				return name
			}
		}
		return ""
	}
	var listed []element
	if json.Unmarshal(doc.Elements, &listed) == nil {
		for i, e := range listed {
			if matches(e) {
				return strconv.Itoa(i)
			}
		}
	}
	return ""
}

func miseTool(raw, path string) string {
	var tools map[string][]struct {
		InstallPath string `json:"install_path"`
	}
	if json.Unmarshal([]byte(raw), &tools) != nil {
		return ""
	}
	for name, versions := range tools {
		for _, v := range versions {
			if v.InstallPath != "" && strings.HasPrefix(path, filepath.Clean(v.InstallPath)+"/") {
				return name
			}
		}
	}
	return ""
}

// ManagerCommands is the fixed argv sequence that updates a package-manager
// owned binary, or nil when tclaude cannot drive that manager.
func ManagerCommands(managedBy, pkg string) [][]string {
	if pkg == "" {
		return nil
	}
	switch managedBy {
	case ManagerHomebrew:
		return [][]string{{"brew", "upgrade", HomebrewFormula}}
	case ManagerNixProfile:
		return [][]string{{"nix", "profile", "upgrade", pkg}}
	case ManagerMise:
		return [][]string{{"mise", "upgrade", pkg}}
	case ManagerDpkg:
		return [][]string{{"sudo", "-n", "apt-get", "update"}, {"sudo", "-n", "apt-get", "install", "--only-upgrade", "-y", pkg}}
	case ManagerRpm:
		return [][]string{{"sudo", "-n", "dnf", "upgrade", "-y", pkg}}
	}
	return nil
}

// UpdateHint is the command the operator can run themselves.
func UpdateHint(managedBy, pkg string) string {
	switch managedBy {
	case ManagerHomebrew:
		return "brew upgrade " + HomebrewFormula
	case ManagerNixProfile:
		return "nix profile upgrade " + pkg
	case ManagerNix:
		return "update tclaude in your Nix configuration (NixOS, home-manager or flake) and rebuild"
	case ManagerMise:
		if pkg == "" {
			pkg = "<tool>"
		}
		return "mise upgrade " + pkg
	case ManagerAsdf:
		return "asdf install tclaude latest, then select it with asdf"
	case ManagerDpkg:
		return "sudo apt-get install --only-upgrade " + pkg
	case ManagerRpm:
		return "sudo dnf upgrade " + pkg
	}
	return ""
}

// UntrackHint is how to remove the package-manager copy before letting
// tclaude manage its own binaries.
func UntrackHint(managedBy, pkg string) string {
	switch managedBy {
	case ManagerHomebrew:
		return "brew uninstall tclaude"
	case ManagerNixProfile:
		return "nix profile remove " + pkg
	case ManagerNix:
		return "remove tclaude from your Nix configuration and rebuild"
	case ManagerMise:
		return "mise uninstall " + pkg
	case ManagerAsdf:
		return "asdf uninstall tclaude"
	case ManagerDpkg:
		return "sudo apt remove " + pkg
	case ManagerRpm:
		return "sudo dnf remove " + pkg
	}
	return ""
}

// Detection is the recommended method for this install, with the evidence.
type Detection struct {
	Method      string `json:"method"`
	Reason      string `json:"reason"`
	ManagedBy   string `json:"managed_by,omitempty"`
	UpdateHint  string `json:"manual_command,omitempty"`
	UntrackHint string `json:"untrack_command,omitempty"`
	// Recognized is false when no detector matched the primary binary.
	Recognized bool     `json:"recognized"`
	BuildInfo  string   `json:"build_info"`
	Binaries   []Binary `json:"binaries"`
}

// MethodInfo describes one choosable method on this host.
type MethodInfo struct {
	ID                string     `json:"id"`
	Label             string     `json:"label"`
	Description       string     `json:"description"`
	Available         bool       `json:"available"`
	UnavailableReason string     `json:"unavailable_reason,omitempty"`
	NotifyOnly        bool       `json:"notify_only"`
	PackageManager    bool       `json:"package_manager"`
	Commands          [][]string `json:"commands,omitempty"`
	// Command is Commands for display, joined with " && ".
	Command string `json:"command,omitempty"`
}

func primary(bins []Binary) Binary {
	for _, b := range bins {
		if b.Name == "tclaude" {
			return b
		}
	}
	if len(bins) > 0 {
		return bins[0]
	}
	return Binary{}
}

// Detect recommends a method. A package-manager-owned or unrecognized install
// is recommended manual: running a package manager is always the operator's
// explicit opt-in.
func Detect(bins []Binary) Detection {
	b := primary(bins)
	d := Detection{Binaries: bins, Method: MethodManual, Recognized: b.Method != "unrecognized" || b.ManagedBy != ""}
	d.BuildInfo = b.Name + " " + b.Version + " (" + b.Method + " build)"
	if b.Version == "" {
		d.BuildInfo = b.Name + " (not an identifiable tclaude Go binary)"
	}
	if b.ManagedBy != "" {
		d.ManagedBy = b.ManagedBy
		d.UpdateHint = UpdateHint(b.ManagedBy, b.Package)
		d.UntrackHint = UntrackHint(b.ManagedBy, b.Package)
		d.Reason = b.Name + " is managed by " + b.ManagedBy + " (" + b.Evidence + "). Update it through " + b.ManagedBy + ": " + d.UpdateHint + "."
		if m := managerMethod[b.ManagedBy]; m != "" && ManagerCommands(b.ManagedBy, b.Package) != nil {
			d.Reason += " You can also let tclaude run it (method " + m + ")."
		}
		return d
	}
	switch b.Method {
	case "release":
		d.Method, d.Reason = MethodRelease, b.Name+" is an official release build ("+b.Path+")."
	case "go_install":
		d.Method, d.Reason = MethodGoInstall, b.Name+" was built with go install ("+b.Path+")."
		if _, err := currentSystem().LookPath("go"); err != nil {
			d.Method, d.Reason = MethodRelease, b.Name+" was built with go install, but go is not on PATH; the release archive is the closest method."
		}
	case "source":
		d.Reason = b.Name + " is a development or checkout build (" + b.Path + "). tclaude will not overwrite it unasked; choose source_checkout to rebuild from your checkout."
	default:
		d.Reason = b.Name + " was not installed by a method tclaude recognizes (" + b.Evidence + "). Choose how it should be updated; manual only notifies."
	}
	return d
}

// Methods lists every method with its availability on this host.
func Methods(bins []Binary) []MethodInfo {
	sys := currentSystem()
	has := func(name string) bool { _, err := sys.LookPath(name); return err == nil }
	managed := map[string]Binary{}
	for _, b := range bins {
		if b.ManagedBy != "" {
			if _, ok := managed[b.ManagedBy]; !ok {
				managed[b.ManagedBy] = b
			}
		}
	}
	sudo := sync.OnceValue(func() bool { _, ok := probe("sudo", "-n", "true"); return ok })
	pm := func(id, manager, tool, label string) MethodInfo {
		info := MethodInfo{ID: id, Label: label, PackageManager: true, Description: "Let tclaude run " + tool + " to update itself. Opt-in; tclaude runs only the fixed command shown."}
		b, ok := managed[manager]
		switch {
		case !ok:
			info.UnavailableReason = "tclaude is not installed through " + tool
		case !has(tool):
			info.UnavailableReason = "needs " + tool + " on PATH"
		case ManagerCommands(manager, b.Package) == nil:
			info.UnavailableReason = "could not determine the " + tool + " package entry"
		case (id == MethodApt || id == MethodDnf) && !sudo():
			info.UnavailableReason = "needs passwordless sudo (sudo -n true failed); run " + UpdateHint(manager, b.Package) + " yourself"
		default:
			info.Available = true
			info.Commands = ManagerCommands(manager, b.Package)
			parts := []string{}
			for _, argv := range info.Commands {
				parts = append(parts, strings.Join(argv, " "))
			}
			info.Command = strings.Join(parts, " && ")
		}
		return info
	}
	release := MethodInfo{ID: MethodRelease, Label: "Download the GitHub release", Description: "Download the official release archive and verify its SHA-256 checksum.", Available: true}
	if _, err := archiveName("tclaude"); err != nil {
		release.Available, release.UnavailableReason = false, err.Error()
	}
	goInstall := MethodInfo{ID: MethodGoInstall, Label: "Build with go install", Description: "Build the release version from source with go install …@<version>.", Available: has("go")}
	if !goInstall.Available {
		goInstall.UnavailableReason = "needs Go on PATH"
	}
	checkout := MethodInfo{ID: MethodSourceCheckout, Label: "Build from a local checkout", Description: "git fetch and check out the release tag (or a branch) in your checkout, then build it. Refuses on uncommitted changes.", Available: has("go") && has("git")}
	if !checkout.Available {
		checkout.UnavailableReason = "needs go and git on PATH"
	}
	return []MethodInfo{
		release, goInstall, checkout,
		pm(MethodHomebrew, ManagerHomebrew, "brew", "Let tclaude run Homebrew"),
		pm(MethodNixProfile, ManagerNixProfile, "nix", "Let tclaude run nix profile upgrade"),
		pm(MethodMise, ManagerMise, "mise", "Let tclaude run mise"),
		pm(MethodApt, ManagerDpkg, "apt-get", "Let tclaude run apt-get (sudo -n)"),
		pm(MethodDnf, ManagerRpm, "dnf", "Let tclaude run dnf (sudo -n)"),
		{ID: MethodCustom, Label: "Run my own update command", Description: "Run your own command with /bin/sh -c. It may use TCLAUDE_UPDATE_VERSION (the latest release); it does not have to. Afterwards tclaude checks the binaries still run and speak the same federation protocol.", Available: true},
		{ID: MethodManual, Label: "Notify me only", Description: "Check for updates and notify; never install.", Available: true, NotifyOnly: true},
	}
}
