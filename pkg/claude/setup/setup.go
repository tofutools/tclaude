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
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/statusbar"
	"github.com/tofutools/tclaude/pkg/common"
)

// defaultPermsForBundledSkills are the low-risk permission slugs the bundled
// agent-* skills exercise. `tclaude setup --install-default-agent-permissions`
// adds them to agent.default_permissions so the agent can use the
// skills without each new conversation needing a manual grant. Kept
// separate from --install-agent-skills so refreshing on-disk skill
// files doesn't re-add slugs the human deliberately revoked.
//
// `self.clear` was removed from the slug registry entirely (along with
// `tclaude agent clear`) because /clear rotates CC's conv ID and
// orphans agent identity. Reincarnate replaces that path.
var defaultPermsForBundledSkills = []string{
	"self.rename",
	"self.compact",
	"self.clone",
	"self.schedule",
	"self.remote-control",
	"self.task",
	"self.pr",
	"self.tags",
	"self.dir-repair",
	"process.templates.read",
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
	InstallAgentSkills       bool `long:"install-agent-skills" help:"Also install (or refresh) the bundled agent-* skills into Claude Code and Codex CLI user skill directories, including CODEX_HOME/skills. Idempotent; overwrites existing if present."`
	InstallDefaultAgentPerms bool `long:"install-default-agent-permissions" help:"Also grant the low-risk permission slugs the bundled agent-* skills exercise as agent defaults in ~/.tclaude/config.json. Idempotent; only adds missing slugs."`
	InstallSandboxHardening  bool `long:"install-sandbox-hardening" help:"Also add the agent-sandbox hardening entries (sandbox.* and permissions.deny) to ~/.claude/settings.json, as described in docs/sandbox-hardening.md. Append-only and idempotent; never removes or overwrites existing values."`
	InstallResumeThreshold   bool `long:"install-resume-threshold-override" help:"Also write a claude_resume.threshold_minutes override to ~/.tclaude/config.json that suppresses Claude Code's interactive 'Resume from summary' prompt for tclaude-spawned panes (it breaks scripted resume). Idempotent; skips if a value is already configured, never overwrites it."`

	// Harness selects which coding harness's hooks to install/check
	// (default "claude" → ~/.claude/settings.json; "codex" →
	// ~/.codex/hooks.json, via the harness HookInstaller seam). Other
	// setup steps keep their own compatibility gates.
	Harness string `long:"harness" optional:"true" help:"Coding harness whose hooks to install: claude (default) | codex"`
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
			"--install-sandbox-hardening, --install-resume-threshold-override. " +
			"--install-all enables every extra.",
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
		return checkStatus(params.Harness)
	}

	// Resolve which harness's hooks to manage (default claude). An
	// unknown value errors up front.
	h, err := harness.Resolve(params.Harness)
	if err != nil {
		return err
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

	// 1. Install hooks — the mandatory core of the integration. Hooks go in
	// for the selected harness (always, regardless of whether its CLI is on
	// PATH) AND are auto-installed for every other registered hook-capable
	// harness whose CLI is present on PATH — so a machine with Codex
	// installed gets its Codex hooks without anyone having to pass
	// `--harness codex`. Installing hooks for a harness you don't actively
	// use is harmless: the status state machine tolerates a harness firing
	// fewer events. A non-selected trust-capable harness gets an explicit prompt;
	// declining leaves both its declarations and trust store untouched.
	for i, hh := range hookInstallTargets(h, harnessOnPath) {
		if i > 0 {
			fmt.Println()
		}
		grantTrust := hh.Name == h.Name
		if !grantTrust {
			if _, trustCapable := hh.Hooks.(harness.TrustedHookInstaller); trustCapable {
				if !consentToDetectedHookTrust(hh, params.Yes) {
					fmt.Printf("• Skipped %s hooks (no hook trust was granted)\n", hh.DisplayName)
					continue
				}
				grantTrust = true
			}
		}
		if err := installHooksForHarness(hh, grantTrust); err != nil {
			return err
		}
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

	// 2a. Claude Code fullscreen TUI. tclaude runs Claude Code inside tmux
	// panes it detaches and reattaches; the classic inline renderer flickers
	// and fights tmux's scrollback, while the fullscreen (alternate-screen)
	// renderer is flicker-free and tmux-friendly — so tclaude only works as
	// intended with it. Offer to set "tui": "fullscreen" in
	// ~/.claude/settings.json, but only on a fresh config: any existing "tui"
	// value is a deliberate choice and is left untouched.
	fmt.Println("\n=== Fullscreen TUI ===")
	configureFullscreenTUI(params)

	// 2a′. AskUserQuestion idle-timeout. Claude Code no longer auto-continues a
	// question dialog by default, so an unattended tclaude-spawned agent stalls
	// on it. Recommend (and, interactively, offer to set) askUserQuestionTimeout
	// in ~/.claude/settings.json — but never under --yes, since it is a global
	// behaviour change; per-agent overrides live in the dashboard spawn dialog.
	fmt.Println("\n=== AskUserQuestion Timeout ===")
	configureAskUserQuestionTimeout(params)

	// 2b. Codex CLI status line (when codex is installed). Codex has no
	// command-backed status line (openai/codex#17827), so tclaude can't
	// install its renderer there; instead it curates Codex's built-in
	// status_line items in ~/.codex/config.toml. Gated on codex being on
	// PATH so non-Codex users never see a prompt. Never clobbers a
	// user-owned status_line.
	fmt.Println("\n=== Codex Status Bar ===")
	switch {
	case !isCodexInstalled():
		fmt.Println("  Codex CLI not found on PATH — skipping (re-run setup after installing codex)")
	default:
		switch statusbar.CodexStatusLineState() {
		case statusbar.CodexInstalledState:
			fmt.Println("✓ Codex status line already configured")
		case statusbar.CodexUserManagedState:
			fmt.Println("  You already have a custom Codex status_line — leaving it untouched.")
		case statusbar.CodexTuiConflictState:
			fmt.Printf("  Your config defines tui as an inline table/array in %s — tclaude won't edit it.\n", statusbar.CodexConfigPath())
			fmt.Println("  Convert tui to a [tui] table (or add the status_line items yourself) and re-run.")
		case statusbar.CodexNeedsManualFixState:
			fmt.Printf("  tclaude's Codex status_line in %s looks hand-edited (unterminated array) — fix or delete it and re-run.\n", statusbar.CodexConfigPath())
		default: // CodexNotInstalled or CodexNeedsRepair
			// A stale tclaude-managed value repairs silently (like hooks); a
			// brand-new install asks first.
			repair := statusbar.CodexStatusLineState() == statusbar.CodexNeedsRepair
			if repair || askYesNo("Install a tclaude-curated status line for Codex CLI?", true, params.Yes) {
				switch outcome, err := statusbar.InstallCodex(); {
				case err != nil:
					fmt.Printf("  Warning: failed to configure Codex status line: %v\n", err)
				case outcome == statusbar.CodexRepaired:
					fmt.Println("✓ Codex status line repaired")
				default:
					fmt.Println("✓ Codex status line installed")
				}
			} else {
				fmt.Println("  Skipped. Install later with: tclaude setup")
			}
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
	configureNotifications(params)

	// 4a. Default the Vegas/slop + wizard-mode music to half volume on a
	// fresh install — full volume startled users on first entry. Skip-if-set,
	// so an operator who already chose a music volume keeps it.
	fmt.Println("\n=== Music Volume ===")
	if err := installDefaultMusicVolume(); err != nil {
		return err
	}

	// 4b. Neutralize the OS-sandbox phantom ".git" in the worktree container.
	// A sandbox that denies writes to ".git" inside a writable root creates a
	// bogus ".git" directory in the sibling-worktree container (e.g. ~/git),
	// which breaks every `go build` under it with "VCS status: exit 128". An
	// empty ".git" placeholder file occupies the path so the sandbox and Go
	// both leave it alone. No-op (and silent) unless setup runs inside a
	// worktree whose grantable container is derivable.
	installWorktreeContainerGitPlaceholder(params.Yes)

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
	if params.InstallResumeThreshold || params.InstallAll {
		fmt.Println("\n=== Resume Threshold Override ===")
		if err := installResumeThresholdOverride(); err != nil {
			return err
		}
	}
	return nil
}

// installResumeThresholdOverride writes a claude_resume.threshold_minutes
// override to ~/.tclaude/config.json that suppresses Claude Code's interactive
// "Resume from summary" chooser — the multiple-choice prompt CC pops when
// resuming an old, large session, which hangs tclaude's scripted resume (a
// detached tmux pane can't answer a TUI it didn't expect). tclaude injects the
// value as the CLAUDE_CODE_RESUME_THRESHOLD_MINUTES env var on the panes it
// spawns; it never touches ~/.claude/settings.json, so manual `claude` runs and
// the dashboard's config diff viewer stay clean.
//
// Skip-if-set, never overwrite: if the operator already configured a
// threshold_minutes (by hand or a previous run), it is left exactly as-is. The
// user opted in by passing the flag (or --install-all); we don't prompt further.
func installResumeThresholdOverride() error {
	cfg, err := config.Load()
	if err != nil {
		// The config file exists but is corrupt/unreadable, so Load fell back to
		// defaults. Saving now would overwrite the file with those defaults,
		// silently discarding whatever the operator's unparseable config held —
		// so skip with a warning rather than clobber it. (A simply-absent file is
		// not an error: Load returns the default config with a nil error, and we
		// proceed to write it.)
		fmt.Printf("  ⚠ Skipping: could not read ~/.tclaude/config.json (%v). Fix it and re-run.\n", err)
		return nil
	}
	if cfg.ClaudeResume != nil && cfg.ClaudeResume.ThresholdMinutes != nil {
		fmt.Printf("✓ claude_resume.threshold_minutes already set (%d) — leaving it unchanged\n",
			*cfg.ClaudeResume.ThresholdMinutes)
		return nil
	}
	if cfg.ClaudeResume == nil {
		cfg.ClaudeResume = &config.ClaudeResumeConfig{}
	}
	v := config.ResumeThresholdMinutesSuppress
	cfg.ClaudeResume.ThresholdMinutes = &v
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Printf("✓ Set claude_resume.threshold_minutes=%d (suppresses Claude Code's resume-from-summary prompt)\n", v)
	return nil
}

// installDefaultMusicVolume writes slop.music_volume=50 to
// ~/.tclaude/config.json so a fresh install starts the Vegas/slop and
// wizard-mode soundtrack at half volume — full volume startled users on first
// entry. The value matches config.DefaultMusicVolume, the same default readers
// fall back to when the key is absent (see ResolvedSlopVolumes); writing it
// here just makes the default explicit and visible in the config / Config tab.
//
// Skip-if-set, never overwrite: if the operator already chose a music volume
// (by hand, the dashboard's 🎚️ mixer, or a previous run) it is left exactly
// as-is. This runs in the always-on baseline, but only ever writes on the
// first run where no music volume exists yet.
func installDefaultMusicVolume() error {
	cfg, err := config.Load()
	if err != nil {
		// The config file exists but is corrupt/unreadable, so Load fell back to
		// defaults. Saving now would overwrite the file with those defaults,
		// silently discarding whatever the operator's unparseable config held —
		// so skip with a warning rather than clobber it. (A simply-absent file is
		// not an error: Load returns the default config with a nil error, and we
		// proceed to write it.)
		fmt.Printf("  ⚠ Skipping: could not read ~/.tclaude/config.json (%v). Fix it and re-run.\n", err)
		return nil
	}
	if cfg.Slop != nil && cfg.Slop.MusicVolume != nil {
		fmt.Printf("✓ slop.music_volume already set (%d%%) — leaving it unchanged\n", *cfg.Slop.MusicVolume)
		return nil
	}
	if cfg.Slop == nil {
		cfg.Slop = &config.SlopConfig{}
	}
	v := config.DefaultMusicVolume
	cfg.Slop.MusicVolume = &v
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Printf("✓ Set slop.music_volume=%d%% (Vegas/slop + wizard-mode soundtrack starts at half volume)\n", v)
	return nil
}

func consentToDetectedHookTrust(h *harness.Harness, assumeYes bool) bool {
	prompt := fmt.Sprintf("Install and trust tclaude hooks for detected %s?", h.DisplayName)
	return askYesNo(prompt, false, assumeYes)
}

// hookInstallTargets returns the harnesses whose tclaude callback hooks the
// baseline should install: the selected harness always (hooks are the
// mandatory core, installed regardless of whether its CLI is on PATH), plus
// every OTHER registered hook-capable harness the `present` predicate
// reports as available — so a Codex install is picked up automatically,
// without `--harness codex`. The selected harness is first; the rest follow
// in registry (name) order. `present` is a parameter so tests can drive the
// auto-add path without depending on what's on the test machine's PATH.
func hookInstallTargets(selected *harness.Harness, present func(*harness.Harness) bool) []*harness.Harness {
	targets := []*harness.Harness{selected}
	seen := map[string]bool{selected.Name: true}
	for _, name := range harness.Names() {
		if seen[name] {
			continue
		}
		h, ok := harness.Get(name)
		if !ok || !h.SupportsHooks() || !present(h) {
			continue
		}
		targets = append(targets, h)
		seen[name] = true
	}
	return targets
}

// harnessOnPath reports whether a harness's launcher binary is resolvable on
// PATH — the gate for auto-installing a NON-selected harness's hooks. A
// harness with no Spawner can't be probed, so it counts as absent. The
// selected harness bypasses this gate entirely (its hooks are mandatory).
func harnessOnPath(h *harness.Harness) bool {
	if h == nil || h.Spawn == nil {
		return false
	}
	_, err := exec.LookPath(h.Spawn.Binary())
	return err == nil
}

// installHooksForHarness installs or repairs the tclaude callback hooks for
// one harness, printing its section. A harness with no HookInstaller in this
// build is skipped with a note rather than failing the whole run.
func installHooksForHarness(h *harness.Harness, grantTrust bool) error {
	fmt.Printf("=== Hooks (%s) ===\n", h.DisplayName)
	if !h.SupportsHooks() {
		fmt.Printf("  (no hook installer for harness %q in this build; skipping)\n", h.Name)
		return nil
	}
	trustedInstaller, hasTrust := h.Hooks.(harness.TrustedHookInstaller)
	trustSupported := false
	trustReason := ""
	if grantTrust && hasTrust {
		trustSupported, trustReason = trustedInstaller.AutoTrustSupported()
	}
	installed, missing, needsRepair := h.Hooks.Check()
	trusted := hasTrust && trustedInstaller.Trusted()
	if installed && !needsRepair && (!grantTrust || !trustSupported || trusted) {
		if hasTrust && trusted {
			fmt.Println("✓ All hooks already installed and trusted")
		} else {
			fmt.Println("✓ All hooks already installed")
		}
	} else {
		if needsRepair {
			fmt.Println("  Repairing hook configuration...")
		}
		if len(missing) > 0 {
			fmt.Printf("  Installing hooks for: %v\n", missing)
		}
		switch {
		case grantTrust && hasTrust && trustSupported && (!installed || needsRepair):
			if err := trustedInstaller.InstallTrusted(); err != nil {
				return fmt.Errorf("failed to install and trust hooks: %w", err)
			}
		case grantTrust && hasTrust && trustSupported:
			if err := trustedInstaller.TrustInstalled(); err != nil {
				return fmt.Errorf("failed to trust installed hooks: %w", err)
			}
		default:
			if err := h.Hooks.Install(); err != nil {
				return fmt.Errorf("failed to install hooks: %w", err)
			}
		}
		trusted = hasTrust && trustedInstaller.Trusted()
		if trusted {
			fmt.Printf("✓ Hooks installed and trusted (%s)\n", h.Hooks.ConfigTarget())
		} else {
			fmt.Printf("✓ Hooks installed (%s)\n", h.Hooks.ConfigTarget())
		}
	}
	if grantTrust && hasTrust && !trustSupported {
		fmt.Printf("  ⚠ Automatic hook trust skipped: %s\n", trustReason)
	}
	if hasTrust && !trusted {
		note := h.Hooks.TrustNote()
		fmt.Printf("  ⚠ %s\n", note)
	}
	// For Codex, also install the tclaude-managed permission profile that lets a
	// sandboxed, daemon-spawned Codex agent reach the agentd Unix socket
	// (JOH-207) — without it a workspace-write agent can't run `tclaude agent …`
	// at all. It is a prerequisite for agent coordination, so it belongs in the
	// baseline alongside the Codex hooks rather than behind an opt-in flag. The
	// spawn path also self-heals it (belt-and-suspenders); installing it here
	// makes it discoverable. A standalone <name>.config.toml file, so it never
	// touches the user's own config.toml. A failure is a warning, not fatal —
	// the spawn-time ensure is the real guarantee.
	if h.Name == harness.CodexName {
		if path, perr := harness.EnsureCodexAgentProfile(); perr != nil {
			fmt.Printf("  ⚠ could not install the agentd permission profile: %v\n", perr)
		} else {
			fmt.Printf("✓ Installed agentd permission profile (%s)\n", path)
		}
	}
	return nil
}

// checkHooksForHarness reports one harness's hook-install status in the
// `tclaude setup --check` output.
func checkHooksForHarness(h *harness.Harness, expectTrust bool) {
	fmt.Printf("\n=== Hooks (%s) ===\n", h.DisplayName)
	if !h.SupportsHooks() {
		fmt.Printf("  (no hook installer for harness %q in this build)\n", h.Name)
		return
	}
	trustedInstaller, hasTrust := h.Hooks.(harness.TrustedHookInstaller)
	installed, missing, needsRepair := h.Hooks.Check()
	if needsRepair {
		fmt.Println("⚠ Hook configuration needs repair")
	}
	if installed {
		if hasTrust && trustedInstaller.Trusted() {
			fmt.Println("✓ All hooks installed and trusted")
		} else {
			fmt.Println("✓ All hooks installed")
		}
	} else {
		fmt.Printf("✗ Missing hooks: %v\n", missing)
	}
	if expectTrust && hasTrust && !trustedInstaller.Trusted() {
		if supported, reason := trustedInstaller.AutoTrustSupported(); !supported {
			fmt.Printf("⚠ Automatic hook trust unavailable: %s\n", reason)
		} else {
			fmt.Println("⚠ Installed hooks are not trusted; run setup to repair")
		}
	}
	// For Codex, also report the managed agentd permission profile (the
	// JOH-207 baseline artifact installed by installHooksForHarness), so a
	// missing/stale profile is surfaced here rather than only at spawn time.
	// Read-only: --check never writes; the spawn path self-heals it.
	if h.Name == harness.CodexName {
		switch path, present, current, perr := harness.CodexAgentProfileStatus(); {
		case perr != nil:
			fmt.Printf("⚠ could not check the agentd permission profile: %v\n", perr)
		case !present:
			fmt.Printf("✗ agentd permission profile missing (%s) — run `tclaude setup` (the spawn path also self-heals it)\n", path)
		case !current:
			fmt.Printf("⚠ agentd permission profile stale (%s) — run `tclaude setup` to refresh\n", path)
		default:
			fmt.Printf("✓ agentd permission profile installed (%s)\n", path)
		}
	}
}

// installAgentSkills writes the bundled skills into user-scope skill
// directories for supported agent harnesses. Idempotent: overwrites existing
// installs.
// The CLI prints each destination so the user knows where to look if
// they want to inspect or edit them locally.
func installAgentSkills() error {
	installed, err := agent.InstallSkills(true)
	if err != nil {
		return fmt.Errorf("install Claude Code agent skills: %w", err)
	}
	for _, s := range installed {
		fmt.Printf("✓ Installed %s skill for Claude Code at %s\n", s.Name, s.Path)
	}
	codexInstalled, err := agent.InstallCodexSkills(true)
	if err != nil {
		return fmt.Errorf("install Codex CLI agent skills: %w", err)
	}
	for _, s := range codexInstalled {
		fmt.Printf("✓ Installed %s skill for Codex CLI at %s\n", s.Name, s.Path)
	}
	fmt.Println("  Run `tclaude agentd serve` (in a non-sandboxed shell) for live delivery.")
	return nil
}

// installDefaultAgentPermissions adds the low-risk permission
// slugs the bundled agent-* skills exercise (defaultPermsForBundledSkills)
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
	for _, slug := range defaultPermsForBundledSkills {
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

// configureNotifications handles the "=== Notifications ===" setup step.
// It respects a notifications block the user has already configured: an
// existing block's Enabled flag is never flipped, so a deliberately
// disabled state survives repeated `tclaude setup` runs (the enable prompt
// and --yes only ever take effect on a genuine first run, when no block
// exists on disk yet). Independently — and per the "additively merge new
// categories" policy — it adds any newly-supported notification categories
// to the transitions list, preserving the user's existing rules, cooldown,
// command and human-message choice.
func configureNotifications(params *Params) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Printf("  Warning: could not load config: %v\n", err)
		cfg = config.DefaultConfig()
	}
	// Probe the raw file BEFORE trusting the normalized block: Load seeds a
	// default (disabled) block when the file has none, so cfg.Notifications
	// is never nil here and can't itself distinguish "user configured this"
	// from "fresh install".
	configured := config.NotificationsPresent()
	if cfg.Notifications == nil { // defensive; Load always normalizes
		cfg.Notifications = config.DefaultConfig().Notifications
	}

	// Additively pick up notification categories added in a newer tclaude
	// version. Everything the user already set is left untouched.
	added := cfg.Notifications.MergeDefaultTypes()

	save := false
	switch {
	case configured && cfg.Notifications.Enabled:
		fmt.Println("✓ Notifications already enabled (keeping your settings)")
		save = len(added) > 0
	case configured:
		// Configured but disabled — the user turned them off on purpose.
		// Never re-enable on a repeat setup; only keep categories current.
		fmt.Println("✓ Notifications disabled in your config — leaving them off")
		save = len(added) > 0
	default:
		// First run: no notifications block on disk yet. Offer to enable.
		if askYesNo("Enable desktop notifications when Claude needs attention?", true, params.Yes) {
			cfg.Notifications.Enabled = true
			save = true
			fmt.Println("✓ Notifications enabled")
		} else {
			fmt.Println("  Notifications not enabled (you can enable later in config)")
		}
	}

	if save && len(added) > 0 {
		noun := "category"
		if len(added) > 1 {
			noun = "categories"
		}
		fmt.Printf("  Added new notification %s: %s\n", noun, strings.Join(added, ", "))
	}

	if save {
		if err := config.Save(cfg); err != nil {
			fmt.Printf("  Warning: could not save config: %v\n", err)
		} else {
			fmt.Printf("  Config saved to: %s\n", config.ConfigPath())
		}
	}
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

func checkStatus(harnessName string) error {
	fmt.Println("tclaude Setup Status")
	fmt.Println()

	h, err := harness.Resolve(harnessName)
	if err != nil {
		return err
	}

	// Check tmux
	fmt.Println("=== Prerequisites ===")
	if isTmuxInstalled() {
		fmt.Println("✓ tmux installed")
	} else {
		fmt.Println("✗ tmux not found (required)")
	}

	// Check hooks. Naming a harness scopes the check to it; with no harness
	// named we mirror what `tclaude setup` installs — the default harness
	// plus every other present hook-capable harness — so `--check` doesn't
	// hide auto-installed Codex hooks.
	checkTargets := []*harness.Harness{h}
	if harnessName == "" {
		checkTargets = hookInstallTargets(h, harnessOnPath)
	}
	for _, hh := range checkTargets {
		checkHooksForHarness(hh, harnessName == "" || hh.Name == h.Name)
	}

	// Check status bar
	fmt.Println("\n=== Status Bar ===")
	if statusbar.CheckInstalled() {
		fmt.Println("✓ Status bar configured")
	} else {
		fmt.Println("✗ Status bar not configured")
		fmt.Println("  Run 'tclaude setup' to install")
	}

	// Check fullscreen TUI
	fmt.Println("\n=== Fullscreen TUI ===")
	checkFullscreenTUI()

	// Check AskUserQuestion idle-timeout
	fmt.Println("\n=== AskUserQuestion Timeout ===")
	checkAskUserQuestionTimeout()

	// Check Codex status line
	fmt.Println("\n=== Codex Status Bar ===")
	if !isCodexInstalled() {
		fmt.Println("  Codex CLI not found on PATH")
	} else {
		switch statusbar.CodexStatusLineState() {
		case statusbar.CodexInstalledState:
			fmt.Println("✓ Codex status line configured")
		case statusbar.CodexNeedsRepair:
			fmt.Println("⚠ Codex status line is tclaude-managed but stale (run 'tclaude setup' to repair)")
		case statusbar.CodexUserManagedState:
			fmt.Println("  Codex status line set by you (not managed by tclaude)")
		case statusbar.CodexTuiConflictState:
			fmt.Println("  tui is defined as an inline table/array (not managed by tclaude)")
		case statusbar.CodexNeedsManualFixState:
			fmt.Println("⚠ tclaude-managed Codex status_line looks hand-edited (unterminated array); fix or delete it and re-run setup")
		default:
			fmt.Println("✗ Codex status line not configured")
			fmt.Println("  Run 'tclaude setup' to install")
		}
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

	// Music volume — report the effective Vegas/slop + wizard-mode music level
	// and whether it's an explicit choice or the resolved default (50%).
	fmt.Println("\n=== Music Volume ===")
	if err != nil {
		fmt.Printf("⚠ Could not load config: %v\n", err)
	} else {
		music, _ := cfg.ResolvedSlopVolumes()
		if cfg.Slop != nil && cfg.Slop.MusicVolume != nil {
			fmt.Printf("✓ slop.music_volume set to %d%%\n", music)
		} else {
			fmt.Printf("✓ slop.music_volume defaults to %d%% (run 'tclaude setup' to write it explicitly)\n", music)
		}
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

// isCodexInstalled checks if the Codex CLI is available on PATH. Used to gate
// the Codex status-line setup so non-Codex users are never prompted.
func isCodexInstalled() bool {
	_, err := exec.LookPath("codex")
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
