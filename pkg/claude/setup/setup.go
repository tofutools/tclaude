// Package setup provides the tclaude setup command for one-time configuration.
package setup

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"sort"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/wsl"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/statusbar"
	"github.com/tofutools/tclaude/pkg/common"
)

// selfPermsForBundledSkills are the permission slugs the bundled
// agent-* skills exercise. `tclaude setup --install-default-agent-permissions`
// adds them to agent.default_permissions so the agent can use the
// skills without each new conversation needing a manual grant. Kept
// separate from --install-agent-skills so refreshing on-disk skill
// files doesn't re-add slugs the human deliberately revoked.
//
// `self.clear` was removed from the slug registry entirely (along with
// `tclaude agent clear`) because /clear rotates CC's conv ID and
// orphans agent identity. Reincarnate replaces that path.
var selfPermsForBundledSkills = []string{
	"self.rename",
	"self.compact",
	"self.reincarnate",
	"self.clone",
	"self.schedule",
}

// Protocol version - bump this when the handler needs to be re-registered
const protocolVersion = "3"

// sandboxHardeningDocPath is the repo-relative path of the operator
// guide for sandboxing agents; sandboxHardeningDocURL is its canonical
// GitHub location, shown in setup output. They are one derived pair so a
// rename only touches the path const — the setup package's tests then
// catch a stale path (doc missing) or out-of-sync in-repo references.
const sandboxHardeningDocPath = "docs/sandbox-hardening.md"

var sandboxHardeningDocURL = "https://github.com/tofutools/tclaude/blob/main/" + sandboxHardeningDocPath

type Params struct {
	Check         bool `short:"c" long:"check" help:"Only check setup status, don't install anything"`
	Force         bool `short:"f" long:"force" help:"Force re-registration of protocol handler"`
	AbsolutePaths bool `long:"absolute-paths" help:"Use absolute paths to tclaude binary in hooks and callbacks"`
	Yes           bool `short:"y" long:"yes" help:"Assume yes on all prompts (for scripted usage)"`
	// The --install-* flags add optional extras on top of the baseline
	// setup (which always runs). They do not replace or gate the baseline.
	InstallAll               bool `long:"install-all" help:"Install every optional extra (equivalent to passing all --install-* flags) on top of the baseline setup."`
	InstallAgentSkills       bool `long:"install-agent-skills" help:"Also install (or refresh) the bundled agent-* skills into ~/.claude/skills/. Idempotent; overwrites existing if present."`
	InstallDefaultAgentPerms bool `long:"install-default-agent-permissions" help:"Also grant the self.* permission slugs the bundled agent-* skills exercise as agent defaults in ~/.tclaude/config.json. Idempotent; only adds missing slugs."`
	InstallSandboxHardening  bool `long:"install-sandbox-hardening" help:"Also add the agent-sandbox hardening entries (sandbox.* and permissions.deny) to ~/.claude/settings.json, as described in docs/sandbox-hardening.md. Append-only and idempotent; never removes or overwrites existing values."`
}

func Cmd() *cobra.Command {
	return boa.CmdT[Params]{
		Use:   "setup",
		Short: "Set up tclaude integration (hooks, protocol handler)",
		Long: "One-time setup for tclaude integration.\n\n" +
			"The baseline setup always runs: it installs hooks in ~/.claude/settings.json, " +
			"configures the status bar, and registers the tclaude:// protocol handler for " +
			"clickable notifications.\n\n" +
			"The --install-* flags add optional extras on top of the baseline (they do not " +
			"replace it): --install-agent-skills, --install-default-agent-permissions, " +
			"--install-sandbox-hardening. --install-all enables every extra.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *Params, cmd *cobra.Command, args []string) {
			if err := runSetup(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runSetup(params *Params) error {
	if params.Check {
		return checkStatus()
	}

	// Configure path mode for hooks and callbacks
	if params.AbsolutePaths {
		clcommon.SetAbsolutePaths(true)
		session.ReinitHookCommand()
		statusbar.ReinitStatusLineCommand()
	}

	// The baseline setup below always runs — hooks, status bar,
	// protocol handler, notifications are the core integration and are
	// never gated behind a flag. The --install-* flags only add
	// optional extras (agent skills, default agent permissions) on top;
	// they are applied by installExtras after the baseline completes.
	fmt.Println("Setting up tclaude integration...")
	fmt.Println()

	// 0. Check tmux. tmux is a hard prerequisite for tclaude itself, so
	// a missing tmux aborts the whole run — baseline AND the --install-*
	// extras. The extras layer on top of the baseline; with no baseline
	// there is nothing to layer onto, and installing skills onto a
	// machine that cannot run tclaude is premature. The user installs
	// tmux and re-runs `tclaude setup`, which then applies the baseline
	// plus any extras idempotently.
	fmt.Println("=== Prerequisites ===")
	if isTmuxInstalled() {
		fmt.Println("✓ tmux installed")
	} else {
		fmt.Println("✗ tmux not found (required for session management)")
		switch runtime.GOOS {
		case "darwin":
			if isBrewInstalled() {
				if askYesNo("Install tmux via Homebrew?", true, params.Yes) {
					fmt.Println("  Installing tmux...")
					if err := installTmux(); err != nil {
						fmt.Printf("  Failed to install: %v\n", err)
						fmt.Println("  Try manually: brew install tmux")
						fmt.Println("\nPlease install tmux and run setup again.")
						return nil
					}
					fmt.Println("✓ tmux installed")
				} else {
					fmt.Println("  Skipped. Install manually: brew install tmux")
					fmt.Println("\nPlease install tmux and run setup again.")
					return nil
				}
			} else {
				fmt.Println("  Homebrew not found. Install tmux manually:")
				fmt.Println("  brew install tmux")
				fmt.Println("\nPlease install tmux and run setup again.")
				return nil
			}
		case "linux":
			fmt.Println("  Install with your package manager:")
			fmt.Println("    Ubuntu/Debian: sudo apt install tmux")
			fmt.Println("    Fedora:        sudo dnf install tmux")
			fmt.Println("    Arch:          sudo pacman -S tmux")
			fmt.Println("\nPlease install tmux and run setup again.")
			return nil
		default:
			fmt.Println("  Please install tmux for your platform.")
			fmt.Println("\nPlease install tmux and run setup again.")
			return nil
		}
	}

	// 1. Install hooks
	fmt.Println("=== Hooks ===")
	installed, missing, needsRepair := session.CheckHooksInstalled()
	if installed && !needsRepair {
		fmt.Println("✓ All hooks already installed")
	} else {
		if needsRepair {
			fmt.Println("  Repairing stale/duplicate hooks...")
		}
		if len(missing) > 0 {
			fmt.Printf("  Installing hooks for: %v\n", missing)
		}
		if err := session.InstallHooks(); err != nil {
			return fmt.Errorf("failed to install hooks: %w", err)
		}
		fmt.Println("✓ Hooks installed")
	}

	// 2. Status bar
	fmt.Println("\n=== Status Bar ===")
	if statusbar.CheckInstalled() {
		fmt.Println("✓ Status bar already configured")
	} else {
		if askYesNo("Install tclaude status bar for Claude Code?", true, params.Yes) {
			if err := statusbar.Install(); err != nil {
				fmt.Printf("  Warning: failed to install status bar: %v\n", err)
			} else {
				fmt.Println("✓ Status bar installed")
			}
		} else {
			fmt.Println("  Skipped. Install later with: tclaude setup")
		}
	}

	// 3. Platform-specific setup for clickable notifications
	fmt.Println("\n=== Clickable Notifications ===")
	if runtime.GOOS == "linux" && wsl.IsWSL() {
		// WSL: Register protocol handler
		registered, err := isProtocolRegistered()
		if err != nil {
			fmt.Printf("  Warning: could not check protocol status: %v\n", err)
		}

		if registered && !params.Force {
			fmt.Println("✓ Protocol handler already registered (tclaude://)")
		} else {
			if params.Force {
				fmt.Println("  Force re-registering protocol handler...")
			}
			if err := registerProtocol(); err != nil {
				fmt.Printf("  Warning: failed to register protocol handler: %v\n", err)
				fmt.Println("  Clickable notifications may not work")
			} else {
				fmt.Println("✓ Protocol handler registered (tclaude://)")
			}
		}
	} else if runtime.GOOS == "darwin" {
		// macOS: Check for terminal-notifier
		if isTerminalNotifierInstalled() {
			fmt.Println("✓ terminal-notifier installed")
		} else {
			fmt.Println("✗ terminal-notifier not found")
			if isBrewInstalled() {
				if askYesNo("Install terminal-notifier via Homebrew?", true, params.Yes) {
					fmt.Println("  Installing terminal-notifier...")
					if err := installTerminalNotifier(); err != nil {
						fmt.Printf("  Failed to install: %v\n", err)
						fmt.Println("  Try manually: brew install terminal-notifier")
					} else {
						fmt.Println("✓ terminal-notifier installed")
					}
				} else {
					fmt.Println("  Skipped. Notifications won't be clickable.")
					fmt.Println("  Install later with: brew install terminal-notifier")
				}
			} else {
				fmt.Println("  Homebrew not found. Install terminal-notifier manually:")
				fmt.Println("  https://github.com/julienXX/terminal-notifier")
				fmt.Println("  Without it, notifications won't be clickable")
			}
		}
	} else if runtime.GOOS == "linux" {
		// Native Linux: Notifications use D-Bus directly, check xdotool /
		// kdotool for window focus — kdotool is the KDE Plasma Wayland
		// drop-in, xdotool covers X11 and XWayland.
		fmt.Println("✓ Notifications use D-Bus (no external tools needed)")
		reportLinuxFocusTools()
	} else if runtime.GOOS == "windows" {
		fmt.Println("  Not implemented for native Windows yet")
	} else {
		fmt.Println("  Not needed on this platform")
	}

	// 4. Configure notifications
	fmt.Println("\n=== Notifications ===")
	cfg, err := config.Load()
	if err != nil {
		fmt.Printf("  Warning: could not load config: %v\n", err)
		cfg = config.DefaultConfig()
	}

	if cfg.Notifications != nil && cfg.Notifications.Enabled {
		fmt.Println("✓ Notifications already enabled")
	} else {
		if askYesNo("Enable desktop notifications when Claude needs attention?", true, params.Yes) {
			if cfg.Notifications == nil {
				cfg.Notifications = config.DefaultConfig().Notifications
			}
			cfg.Notifications.Enabled = true
			if err := config.Save(cfg); err != nil {
				fmt.Printf("  Warning: could not save config: %v\n", err)
			} else {
				fmt.Println("✓ Notifications enabled")
				fmt.Printf("  Config saved to: %s\n", config.ConfigPath())
			}
		} else {
			fmt.Println("  Notifications not enabled (you can enable later in config)")
		}
	}

	// 5. Optional extras layered on top of the baseline.
	if err := installExtras(params); err != nil {
		return err
	}

	// Operator advisory: surface the agent-sandbox hardening doc. This
	// is an unconditional pointer, not a detection — see sandboxAdvisory.
	fmt.Println(sandboxAdvisory())

	fmt.Println("\n=== Setup Complete ===")
	fmt.Println("You can verify with: tclaude setup --check")

	return nil
}

// installExtras runs the optional, flag-gated installs that layer on
// top of the always-run baseline setup: the bundled agent-* skills,
// their default agent permissions, and the agent-sandbox hardening
// entries in ~/.claude/settings.json. Each --install-* flag selects one
// extra; --install-all selects every extra. With no flags set this is
// a no-op, so the baseline-only `tclaude setup` is unaffected.
func installExtras(params *Params) error {
	if params.InstallAgentSkills || params.InstallAll {
		fmt.Println("\n=== Agent Skills ===")
		if err := installAgentSkills(); err != nil {
			return err
		}
	}
	if params.InstallDefaultAgentPerms || params.InstallAll {
		fmt.Println("\n=== Default Agent Permissions ===")
		if err := installDefaultAgentPermissions(); err != nil {
			return err
		}
	}
	if params.InstallSandboxHardening || params.InstallAll {
		fmt.Println("\n=== Sandbox Hardening ===")
		if err := installSandboxHardening(); err != nil {
			return err
		}
	}
	return nil
}

// installAgentSkills writes the bundled agent-* skills into
// ~/.claude/skills/<name>/. Idempotent: overwrites existing installs.
// The CLI prints each destination so the user knows where to look if
// they want to inspect or edit them locally.
func installAgentSkills() error {
	installed, err := agent.InstallSkills(true)
	if err != nil {
		return fmt.Errorf("install agent skills: %w", err)
	}
	for _, s := range installed {
		fmt.Printf("✓ Installed %s skill at %s\n", s.Name, s.Path)
	}
	fmt.Println("  Run `tclaude agentd serve` (in a non-sandboxed shell) for live delivery.")
	return nil
}

// installDefaultAgentPermissions adds the self-targeted permission
// slugs the bundled agent-* skills exercise (selfPermsForBundledSkills)
// to agent.default_permissions in ~/.tclaude/config.json, creating the
// section if missing. Idempotent — slugs already present are silently
// skipped. The user explicitly opted in by passing the flag (or
// --install-all); we don't prompt further.
func installDefaultAgentPermissions() error {
	cfg, _ := config.Load()
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	if cfg.Agent == nil {
		cfg.Agent = &config.AgentConfig{}
	}
	var added []string
	for _, slug := range selfPermsForBundledSkills {
		if !slices.Contains(cfg.Agent.DefaultPermissions, slug) {
			cfg.Agent.DefaultPermissions = append(cfg.Agent.DefaultPermissions, slug)
			added = append(added, slug)
		}
	}
	if len(added) == 0 {
		fmt.Println("✓ All bundled-skill default permissions already granted")
		return nil
	}
	sort.Strings(cfg.Agent.DefaultPermissions)
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	for _, slug := range added {
		fmt.Printf("✓ Granted default permission %s\n", slug)
	}
	return nil
}

// sandboxAdvisory returns the operator-facing note printed at the end of
// `tclaude setup` and `tclaude setup --check`.
//
// It is deliberately an *unconditional pointer*, not a detection. Whether
// the operator's Claude Code sandbox actually denies agents direct access
// to ~/.tclaude and ~/.claude/sessions cannot be reliably determined from
// tclaude's side: it depends on settings.json keys (`sandbox.*` and
// `permissions.deny`) that merge across user, project, and managed
// scopes — most of which tclaude setup never sees — and on Claude Code's
// Bash-sandbox-vs-file-tool split. A detection-based warning would be
// false-positive-prone, and a wrong warning is worse than none, so this
// surfaces the hardening doc and leaves the verdict to the operator.
func sandboxAdvisory() string {
	return "\n=== Agent Sandbox ===\n" +
		"ℹ If you run Claude Code agents through tclaude (agentd / `tclaude agent`),\n" +
		"  make sure your Claude Code sandbox denies agents direct access to\n" +
		"  tclaude's daemon state:\n" +
		"    ~/.tclaude          session, group, and permission state\n" +
		"    ~/.claude/sessions  per-process identity files agentd trusts\n" +
		"  agentd's permission gating is a coordination guardrail, not a security\n" +
		"  boundary — an agent that can edit those files bypasses it entirely.\n" +
		"  tclaude can't verify this for you; it depends on your Claude Code\n" +
		"  settings.json. See:\n" +
		"  " + sandboxHardeningDocURL
}

// askYesNo prompts the user for a yes/no answer. If assumeYes is true, prints the prompt and returns true without reading input.
func askYesNo(prompt string, defaultYes bool, assumeYes bool) bool {
	if assumeYes {
		fmt.Printf("%s [y]: yes\n", prompt)
		return true
	}

	reader := bufio.NewReader(os.Stdin)

	defaultStr := "Y/n"
	if !defaultYes {
		defaultStr = "y/N"
	}

	fmt.Printf("%s [%s]: ", prompt, defaultStr)
	input, err := reader.ReadString('\n')
	if err != nil {
		return defaultYes
	}

	input = strings.TrimSpace(strings.ToLower(input))
	if input == "" {
		return defaultYes
	}

	return input == "y" || input == "yes"
}

func checkStatus() error {
	fmt.Println("tclaude Setup Status")
	fmt.Println()

	// Check tmux
	fmt.Println("=== Prerequisites ===")
	if isTmuxInstalled() {
		fmt.Println("✓ tmux installed")
	} else {
		fmt.Println("✗ tmux not found (required)")
	}

	// Check hooks
	fmt.Println("\n=== Hooks ===")
	installed, missing, needsRepair := session.CheckHooksInstalled()
	if needsRepair {
		fmt.Println("⚠ Stale or duplicate hooks detected (need repair)")
	}
	if installed {
		fmt.Println("✓ All hooks installed")
	} else {
		fmt.Printf("✗ Missing hooks: %v\n", missing)
	}

	// Check status bar
	fmt.Println("\n=== Status Bar ===")
	if statusbar.CheckInstalled() {
		fmt.Println("✓ Status bar configured")
	} else {
		fmt.Println("✗ Status bar not configured")
		fmt.Println("  Run 'tclaude setup' to install")
	}

	// Check clickable notifications setup
	fmt.Println("\n=== Clickable Notifications ===")
	if runtime.GOOS == "linux" && wsl.IsWSL() {
		registered, err := isProtocolRegistered()
		if err != nil {
			fmt.Printf("⚠ Could not check: %v\n", err)
		} else if registered {
			fmt.Println("✓ Protocol handler registered (tclaude://)")
		} else {
			fmt.Println("✗ Protocol handler not registered")
		}
	} else if runtime.GOOS == "darwin" {
		if isTerminalNotifierInstalled() {
			fmt.Println("✓ terminal-notifier installed")
		} else {
			fmt.Println("✗ terminal-notifier not installed")
			fmt.Println("  Install with: brew install terminal-notifier")
		}
	} else if runtime.GOOS == "linux" {
		// Native Linux
		fmt.Println("✓ Notifications use D-Bus (no external tools needed)")
		reportLinuxFocusTools()
	} else if runtime.GOOS == "windows" {
		fmt.Println("  Not implemented for native Windows yet")
	} else {
		fmt.Println("  Not applicable on this platform")
	}

	// Check config and notifications
	fmt.Println("\n=== Notifications ===")
	cfg, err := config.Load()
	if err != nil {
		fmt.Printf("⚠ Could not load config: %v\n", err)
	} else if cfg.Notifications != nil && cfg.Notifications.Enabled {
		fmt.Println("✓ Notifications enabled")
		fmt.Printf("  Config: %s\n", config.ConfigPath())
	} else {
		fmt.Println("✗ Notifications disabled")
		fmt.Printf("  Run 'tclaude setup' to enable\n")
	}

	fmt.Println(sandboxAdvisory())

	return nil
}

// isProtocolRegistered checks if the tclaude:// protocol handler is registered with current version.
func isProtocolRegistered() (bool, error) {
	psPath := wsl.FindPowerShell()
	if psPath == "" {
		return false, fmt.Errorf("powershell not found")
	}

	checkScript := fmt.Sprintf(`
$key = Get-ItemProperty -Path 'HKCU:\Software\Classes\tclaude' -ErrorAction SilentlyContinue
if ($key -and $key.Version -eq '%s') { Write-Output 'registered' }
`, protocolVersion)

	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", checkScript)
	output, err := cmd.Output()
	if err != nil {
		return false, err
	}

	return strings.Contains(string(output), "registered"), nil
}

// registerProtocol registers the tclaude:// protocol handler on Windows via WSL.
func registerProtocol() error {
	psPath := wsl.FindPowerShell()
	if psPath == "" {
		return fmt.Errorf("powershell not found")
	}

	// Register the protocol handler
	// The handler extracts session ID from tclaude://focus/SESSION_ID and calls wsl to run tclaude focus
	registerScript := fmt.Sprintf(`
$ErrorActionPreference = 'SilentlyContinue'

# Create protocol key with all required values
New-Item -Path 'HKCU:\Software\Classes\tclaude' -Force | Out-Null
Set-ItemProperty -Path 'HKCU:\Software\Classes\tclaude' -Name '(Default)' -Value 'URL:tclaude Protocol'
Set-ItemProperty -Path 'HKCU:\Software\Classes\tclaude' -Name 'URL Protocol' -Value ''
Set-ItemProperty -Path 'HKCU:\Software\Classes\tclaude' -Name 'Version' -Value '%s'

# Add DefaultIcon (uses Windows Terminal icon if available)
New-Item -Path 'HKCU:\Software\Classes\tclaude\DefaultIcon' -Force | Out-Null
$wtPath = (Get-Command wt.exe -ErrorAction SilentlyContinue).Source
if ($wtPath) {
    Set-ItemProperty -Path 'HKCU:\Software\Classes\tclaude\DefaultIcon' -Name '(Default)' -Value "$wtPath,0"
}

# Create shell/open/command key
New-Item -Path 'HKCU:\Software\Classes\tclaude\shell\open\command' -Force | Out-Null

# The command extracts session ID and calls wsl to run tclaude focus
# %%1 will be like: tclaude://focus/abc12345
$cmd = 'powershell.exe -NoProfile -WindowStyle Hidden -Command "$url = ''%%1''; $sessionId = $url -replace ''tclaude://focus/'',''''; wsl -- tclaude session focus $sessionId"'
Set-ItemProperty -Path 'HKCU:\Software\Classes\tclaude\shell\open\command' -Name '(Default)' -Value $cmd

Write-Output 'OK'
`, protocolVersion)

	cmd := exec.Command(psPath, "-NoProfile", "-NonInteractive", "-Command", registerScript)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("registration failed: %v\nOutput: %s", err, string(output))
	}

	if !strings.Contains(string(output), "OK") {
		return fmt.Errorf("registration may have failed: %s", string(output))
	}

	return nil
}

// IsProtocolRegistered is exported for use by the notify package.
func IsProtocolRegistered() bool {
	if runtime.GOOS != "linux" || !wsl.IsWSL() {
		return false
	}
	registered, _ := isProtocolRegistered()
	return registered
}

// isTerminalNotifierInstalled checks if terminal-notifier is available on macOS.
func isTerminalNotifierInstalled() bool {
	_, err := exec.LookPath("terminal-notifier")
	return err == nil
}

// isBrewInstalled checks if Homebrew is available.
func isBrewInstalled() bool {
	_, err := exec.LookPath("brew")
	return err == nil
}

// installTerminalNotifier installs terminal-notifier via Homebrew.
func installTerminalNotifier() error {
	cmd := exec.Command("brew", "install", "terminal-notifier")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// installTmux installs tmux via Homebrew.
func installTmux() error {
	cmd := exec.Command("brew", "install", "tmux")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// isTmuxInstalled checks if tmux is available.
func isTmuxInstalled() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// reportLinuxFocusTools prints the window-focus tool status for both
// setup paths (install + --check). It distinguishes "either tool is
// enough" (the common case) from "neither is installed", and tailors
// the install hint to the host's session type:
//
//   - KDE Plasma Wayland → push kdotool (the only tool that sees
//     native-Wayland Plasma windows).
//   - Non-KDE Wayland (GNOME / Sway / Hyprland) → push xdotool only.
//     kdotool refuses to run on non-KDE (upstream "Unsupported KDE
//     version") so suggesting it would just frustrate the user.
//   - X11 → push xdotool (older, battle-tested).
//   - Ambiguous (SSH, headless) → mention both, point at xdotool
//     first for distro packageability.
func reportLinuxFocusTools() {
	xdo := session.IsXdotoolInstalled()
	kdo := session.IsKdotoolInstalled()
	switch {
	case xdo && kdo:
		fmt.Println("✓ xdotool + kdotool installed (window focus on X11 and Wayland)")
	case xdo:
		fmt.Println("✓ xdotool installed (for window focus on X11 / XWayland)")
		if isWaylandSession() && isKDEDesktop() {
			fmt.Println("  Tip: KDE Plasma Wayland — install kdotool for native-Wayland focus")
			printKdotoolInstallHint("       ")
		}
	case kdo:
		fmt.Println("✓ kdotool installed (for window focus on KDE Plasma Wayland / X11)")
	default:
		fmt.Println("✗ no window-focus tool found (optional)")
		switch {
		case isWaylandSession() && isKDEDesktop():
			fmt.Println("  Install kdotool for KDE Plasma Wayland:")
			printKdotoolInstallHint("    ")
		case isWaylandSession():
			// Non-KDE Wayland: there isn't a great option. xdotool
			// helps for any XWayland apps; native-Wayland focus on
			// GNOME/Sway/Hyprland has no tclaude story today.
			fmt.Println("  Install xdotool (covers X11 + XWayland apps):")
			fmt.Println("    sudo apt install xdotool")
			fmt.Println("  Native-Wayland focus is not currently supported on " +
				"GNOME / Sway / Hyprland.")
		default:
			fmt.Println("  Install xdotool (X11): sudo apt install xdotool")
			fmt.Println("  Or, on KDE Plasma Wayland, kdotool:")
			printKdotoolInstallHint("    ")
		}
	}
}

// printKdotoolInstallHint prints the kdotool install pointer at the
// given indent. kdotool is not distro-packaged on most distros; the
// upstream install paths are `cargo install kdotool` (which requires
// the Rust toolchain — easy to forget) and the prebuilt binaries from
// the GitHub releases page. Both are surfaced so a user without Rust
// has somewhere to go.
func printKdotoolInstallHint(indent string) {
	fmt.Println(indent + "cargo install kdotool   (requires Rust toolchain)")
	fmt.Println(indent + "or prebuilt binaries: https://github.com/jinliu/kdotool/releases")
}

// isWaylandSession reports whether the current login session looks
// like Wayland — WAYLAND_DISPLAY is the authoritative signal, falling
// back to XDG_SESSION_TYPE when WAYLAND_DISPLAY is unset (e.g. an SSH
// shell where the env hasn't been propagated). Only used to tailor
// install hints; the runtime focus dispatcher does its own resolution.
func isWaylandSession() bool {
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		return true
	}
	return strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland")
}

// isKDEDesktop reports whether the host's desktop environment is KDE
// Plasma — matched against XDG_CURRENT_DESKTOP (colon-separated per
// spec) and KDE_SESSION_VERSION. Used only by reportLinuxFocusTools to
// decide which install hint to surface; the runtime focus resolver
// does its own KDE detection (see session.pickPreferredFocusTool).
func isKDEDesktop() bool {
	if os.Getenv("KDE_SESSION_VERSION") != "" {
		return true
	}
	for _, part := range strings.Split(os.Getenv("XDG_CURRENT_DESKTOP"), ":") {
		if strings.EqualFold(strings.TrimSpace(part), "KDE") {
			return true
		}
	}
	return false
}
