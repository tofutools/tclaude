// Package doctor diagnoses what this host and this install still need for
// tclaude to work properly: missing tools, incomplete setup, and decisions only
// the operator can make. The dashboard's Doctor tab renders the result.
//
// Every check is read-only. A fix is described (a command to copy, or a
// dashboard action the operator triggers), never applied here.
package doctor

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/statusbar"
)

// Severity orders how loudly a check asks for attention.
type Severity string

const (
	SeverityOK       Severity = "ok"
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityBlocking Severity = "blocking"
	// SeverityDecision is a question only the operator can answer. It is not a
	// fault: either answer is valid, tclaude just needs one.
	SeverityDecision Severity = "decision"
)

// Category groups checks in the Doctor tab's sidebar.
type Category string

const (
	CategoryDecisions Category = "decisions"
	CategorySandbox   Category = "sandbox"
	CategoryTools     Category = "tools"
	CategoryHarnesses Category = "harnesses"
	CategorySetup     Category = "setup"
)

// ActionSandboxDefault asks the dashboard to offer the default-sandbox choice
// (PUT /api/sandbox-impl-default with tclaude-layer or harness).
const ActionSandboxDefault = "sandbox-default"

// Check is one diagnosed item.
type Check struct {
	ID       string   `json:"id"`
	Category Category `json:"category"`
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`
	Detail   string   `json:"detail,omitempty"`
	// Fix is a shell command the operator can copy and run.
	Fix string `json:"fix,omitempty"`
	// Action names an in-dashboard control that resolves the check.
	Action string `json:"action,omitempty"`
	// Value is the current setting for an Action check, so the control can
	// show which option is in effect.
	Value string `json:"value,omitempty"`
}

// Probes are the host-touching predicates, indirected so tests can describe a
// host without owning it.
type Probes struct {
	LookPath             func(string) (string, error)
	SandboxDefault       func() (value string, decided bool, err error)
	TclaudeLayerPresence func() error
	// TclaudeLayerAvailability is the live probe a launch runs; it forks bwrap
	// on Linux, so callers cache Run's result rather than polling it.
	TclaudeLayerAvailability func() error
	CopilotPosture           func(*harness.Harness) error
	StatusBarInstalled       func() bool
	OSRelease                func() map[string]string
}

// HostProbes are the real predicates.
func HostProbes() Probes {
	return Probes{
		LookPath:                 exec.LookPath,
		SandboxDefault:           db.GetSandboxDefaultImplementation,
		TclaudeLayerPresence:     session.TclaudeLayerHostToolingPresence,
		TclaudeLayerAvailability: session.TclaudeLayerHostAvailability,
		CopilotPosture: func(h *harness.Harness) error {
			return session.ValidateTclaudeLayerHarnessPosture(h, nil, nil)
		},
		StatusBarInstalled: statusbar.CheckInstalled,
		OSRelease:          readOSRelease,
	}
}

// Run evaluates every check.
func Run(p Probes) []Check {
	var checks []Check
	checks = append(checks, sandboxChecks(p)...)
	checks = append(checks, toolChecks(p)...)
	checks = append(checks, harnessChecks(p)...)
	checks = append(checks, setupChecks(p)...)
	return checks
}

func sandboxChecks(p Probes) []Check {
	value, decided, err := p.SandboxDefault()
	usesLayer := decided && value == db.SandboxDefaultTclaudeLayer
	var checks []Check
	switch {
	case err != nil:
		checks = append(checks, Check{
			ID: "sandbox-default", Category: CategoryDecisions, Severity: SeverityWarning,
			Title:  "Default sandbox setting is unreadable",
			Detail: fmt.Sprintf("%v. New agents fall back to each harness's own sandbox until you choose again.", err),
			Action: ActionSandboxDefault,
		})
	case !decided:
		checks = append(checks, Check{
			ID: "sandbox-default", Category: CategoryDecisions, Severity: SeverityDecision,
			Title: "Choose the default sandbox for new agents",
			Detail: "New installs confine agents in tclaude's own sandbox: one wall that works the same for every harness " +
				"and enforces your sandbox profiles. This install still uses each harness's own sandbox. " +
				"Existing agents keep the sandbox they were launched with.",
			Action: ActionSandboxDefault,
		})
	default:
		title := "New agents use tclaude's sandbox"
		if !usesLayer {
			title = "New agents use each harness's own sandbox"
		}
		checks = append(checks, Check{
			ID: "sandbox-default", Category: CategorySandbox, Severity: SeverityOK,
			Title: title, Action: ActionSandboxDefault, Value: value,
		})
	}

	// When the layer is not the default it is optional, so a host that cannot
	// run it is information, not a fault.
	missing := SeverityInfo
	consequence := "Only needed if you choose tclaude's sandbox (by default or in a spawn profile)."
	if usesLayer {
		missing = SeverityBlocking
		consequence = "tclaude's sandbox is your default, so agent spawns are refused until this is fixed."
	}
	switch runtime.GOOS {
	case "linux":
		if _, err := p.LookPath("bwrap"); err != nil {
			checks = append(checks, Check{
				ID: "bwrap", Category: CategorySandbox, Severity: missing,
				Title:  "bubblewrap (bwrap) is not installed",
				Detail: "tclaude's sandbox uses bubblewrap on Linux. " + consequence,
				Fix:    packageInstallCommand(p.OSRelease(), "bubblewrap"),
			})
			break
		}
		if err := p.TclaudeLayerPresence(); err != nil {
			checks = append(checks, Check{
				ID: "tclaude-layer", Category: CategorySandbox, Severity: missing,
				Title: "tclaude's sandbox is missing a host capability", Detail: err.Error() + ". " + consequence,
			})
			break
		}
		if err := p.TclaudeLayerAvailability(); err != nil {
			checks = append(checks, Check{
				ID: "tclaude-layer", Category: CategorySandbox, Severity: missing,
				Title: "bubblewrap is installed but cannot create a sandbox",
				Detail: err.Error() + ". This usually means unprivileged user namespaces are disabled — on Ubuntu 23.10+ " +
					"by AppArmor (kernel.apparmor_restrict_unprivileged_userns). See docs/sandboxing.md. " + consequence,
			})
			break
		}
		checks = append(checks, Check{ID: "tclaude-layer", Category: CategorySandbox, Severity: SeverityOK,
			Title: "tclaude's sandbox can run on this host (bubblewrap)"})
	case "darwin":
		if err := p.TclaudeLayerPresence(); err != nil {
			checks = append(checks, Check{
				ID: "tclaude-layer", Category: CategorySandbox, Severity: missing,
				Title: "tclaude's sandbox is unavailable", Detail: err.Error() + ". " + consequence,
			})
			break
		}
		checks = append(checks, Check{ID: "tclaude-layer", Category: CategorySandbox, Severity: SeverityOK,
			Title: "tclaude's sandbox can run on this host (Seatbelt)"})
	default:
		checks = append(checks, Check{
			ID: "tclaude-layer", Category: CategorySandbox, Severity: missing,
			Title:  "tclaude's sandbox is not supported on " + runtime.GOOS,
			Detail: "It runs on Linux and macOS. " + consequence,
		})
	}

	// Copilot is the one harness whose own sandbox tclaude cannot switch off per
	// launch, so tclaude's sandbox refuses unless Copilot's settings leave it off.
	if usesLayer {
		if copilot, ok := harness.Get(harness.CopilotName); ok && onPath(p, copilot) {
			if err := p.CopilotPosture(copilot); err != nil {
				checks = append(checks, Check{
					ID: "copilot-posture", Category: CategorySandbox, Severity: SeverityWarning,
					Title:  "Copilot agents will be refused under tclaude's sandbox",
					Detail: err.Error(),
				})
			}
		}
	}
	return checks
}

func toolChecks(p Probes) []Check {
	var checks []Check
	if _, err := p.LookPath("tmux"); err != nil {
		checks = append(checks, Check{
			ID: "tmux", Category: CategoryTools, Severity: SeverityBlocking,
			Title: "tmux is not installed", Detail: "Every tclaude session runs in tmux.",
			Fix: packageInstallCommand(p.OSRelease(), "tmux"),
		})
	} else {
		checks = append(checks, Check{ID: "tmux", Category: CategoryTools, Severity: SeverityOK, Title: "tmux is installed"})
	}
	if _, err := p.LookPath("git"); err != nil {
		checks = append(checks, Check{
			ID: "git", Category: CategoryTools, Severity: SeverityWarning,
			Title: "git is not installed", Detail: "Worktrees, branch and PR status need git.",
			Fix: packageInstallCommand(p.OSRelease(), "git"),
		})
	} else {
		checks = append(checks, Check{ID: "git", Category: CategoryTools, Severity: SeverityOK, Title: "git is installed"})
	}
	return checks
}

func harnessChecks(p Probes) []Check {
	var checks []Check
	installed := 0
	for _, name := range harness.Names() {
		h, ok := harness.Get(name)
		if !ok || h.Name == harness.ShellName || h.Spawn == nil || !h.Spawnable() {
			continue
		}
		if onPath(p, h) {
			installed++
			checks = append(checks, Check{ID: "harness-" + h.Name, Category: CategoryHarnesses, Severity: SeverityOK,
				Title: h.DisplayName + " is installed"})
			continue
		}
		checks = append(checks, Check{ID: "harness-" + h.Name, Category: CategoryHarnesses, Severity: SeverityInfo,
			Title:  h.DisplayName + " is not installed",
			Detail: fmt.Sprintf("Optional. Install it to spawn %s agents (`%s` on PATH).", h.DisplayName, h.Spawn.Binary())})
	}
	if installed == 0 {
		checks = append(checks, Check{
			ID: "harness-none", Category: CategoryHarnesses, Severity: SeverityBlocking,
			Title:  "No coding harness is installed",
			Detail: "tclaude wraps a harness such as Claude Code or Codex CLI; install at least one.",
		})
	}
	return checks
}

func setupChecks(p Probes) []Check {
	var checks []Check
	for _, name := range harness.Names() {
		h, ok := harness.Get(name)
		if !ok || !onPath(p, h) || h.StatusExtension != nil || !h.SupportsHooks() {
			continue
		}
		installed, missing, needsRepair := h.Hooks.Check()
		switch {
		case needsRepair:
			checks = append(checks, Check{ID: "hooks-" + h.Name, Category: CategorySetup, Severity: SeverityWarning,
				Title: h.DisplayName + " hooks need repair", Detail: "Live status and idle detection may be wrong.",
				Fix: "tclaude setup"})
		case !installed:
			checks = append(checks, Check{ID: "hooks-" + h.Name, Category: CategorySetup, Severity: SeverityWarning,
				Title:  h.DisplayName + " hooks are not installed",
				Detail: fmt.Sprintf("Without them, status, context and idle detection are degraded (missing: %s).", strings.Join(missing, ", ")),
				Fix:    "tclaude setup"})
		default:
			checks = append(checks, Check{ID: "hooks-" + h.Name, Category: CategorySetup, Severity: SeverityOK,
				Title: h.DisplayName + " hooks are installed"})
		}
	}
	if p.StatusBarInstalled() {
		checks = append(checks, Check{ID: "statusbar", Category: CategorySetup, Severity: SeverityOK, Title: "Status bar is configured"})
	} else {
		checks = append(checks, Check{ID: "statusbar", Category: CategorySetup, Severity: SeverityInfo,
			Title: "Status bar is not configured", Detail: "Optional: shows context, cost and quota in the harness status line.",
			Fix: "tclaude setup"})
	}
	return checks
}

func onPath(p Probes, h *harness.Harness) bool {
	if h == nil || h.Spawn == nil {
		return false
	}
	_, err := p.LookPath(h.Spawn.Binary())
	return err == nil
}

// packageInstallCommand suggests the host's package-manager command, or "" when
// the host is not one it recognizes — a wrong command is worse than none.
func packageInstallCommand(osRelease map[string]string, pkg string) string {
	if runtime.GOOS == "darwin" {
		if pkg == "bubblewrap" {
			return ""
		}
		return "brew install " + pkg
	}
	ids := strings.Fields(osRelease["ID"] + " " + osRelease["ID_LIKE"])
	for _, id := range ids {
		switch id {
		case "debian", "ubuntu":
			return "sudo apt install " + pkg
		case "fedora", "rhel", "centos":
			return "sudo dnf install " + pkg
		case "arch":
			return "sudo pacman -S " + pkg
		case "opensuse", "suse":
			return "sudo zypper install " + pkg
		case "alpine":
			return "sudo apk add " + pkg
		}
	}
	return ""
}

func readOSRelease() map[string]string {
	out := map[string]string{}
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if ok {
			out[key] = strings.Trim(value, `"'`)
		}
	}
	return out
}
