package setup

import (
	"fmt"
	"os/exec"
	"strings"
)

// linuxSandboxTools are the host binaries Claude Code's Linux sandbox
// (sandbox.enabled / `--sandbox on`) needs: bubblewrap builds the
// filesystem/namespace jail and socat bridges the sandbox's network proxy
// sockets. Without either, Claude Code refuses to start sandboxed. macOS
// needs neither — Claude Code sandboxes there via the built-in Seatbelt
// (sandbox-exec) — so this check is Linux-only.
var linuxSandboxTools = []struct{ bin, pkg string }{
	{"bwrap", "bubblewrap"},
	{"socat", "socat"},
}

// lookPathSandboxTool is swappable for tests.
var lookPathSandboxTool = exec.LookPath

// missingLinuxSandboxPackages returns the distro package names of the
// sandbox tools not found on PATH.
func missingLinuxSandboxPackages() []string {
	var missing []string
	for _, t := range linuxSandboxTools {
		if _, err := lookPathSandboxTool(t.bin); err != nil {
			missing = append(missing, t.pkg)
		}
	}
	return missing
}

// reportLinuxSandboxTools prints the sandbox-tool status and, when any are
// missing, a package-manager install hint. It is advisory, not fatal like
// the tmux check: tclaude runs fine unsandboxed.
func reportLinuxSandboxTools() {
	missing := missingLinuxSandboxPackages()
	if len(missing) == 0 {
		fmt.Println("✓ bubblewrap + socat installed (for the Claude Code sandbox)")
		return
	}
	pkgs := strings.Join(missing, " ")
	fmt.Printf("✗ %s not found (needed for the Claude Code sandbox)\n", strings.Join(missing, " + "))
	fmt.Println("  Install with your package manager:")
	fmt.Printf("    Ubuntu/Debian: sudo apt install %s\n", pkgs)
	fmt.Printf("    Fedora:        sudo dnf install %s\n", pkgs)
	fmt.Printf("    Arch:          sudo pacman -S %s\n", pkgs)
}
