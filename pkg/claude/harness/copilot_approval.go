package harness

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Copilot's approval catalog. Every flag rendered from a token here is one the
// pinned-1.0.77 permission matrix MEASURED against the real binary
// (copilotfixture/testdata/1.0.77/permission_contract.json). That restriction
// is the whole point of the file rather than a stylistic preference: the plan
// this catalog replaces proposed a default containing `--deny-tool 'url()'`,
// and the measurement showed that spelling is rejected at argument parse with
// exit 1 and no provider contact — a default that would have killed every
// Copilot pane at launch. So a documented flag is not enough here; the
// contract entry that measured each one is named beside it below.
//
// Copilot's permission surface is several INDEPENDENT prompt sources, not one,
// and a token is honest only about the ones it actually closes:
//
//   - tool approval — closed by --allow-all-tools (contract:
//     default-interactive-blocking).
//   - the ask_user tool — closed by --no-ask-user (contract: no-ask-user).
//   - URL access from the SHELL tool — also closed by --allow-all-tools
//     (contract: url-access, which corrected the plan's assumption that this
//     needed its own deny rule). URL access from Copilot's web_fetch tool is
//     NOT measured: the hermetic lab runs under COPILOT_OFFLINE=true, which
//     removes web_fetch from the catalog entirely, so no token here claims
//     anything about it in either direction.
//   - directory access — its own "Allow directory access" dialog, closed for a
//     named directory by --add-dir (contract: out-of-cwd-paths).
//   - folder trust — the FIRST gate, before the provider is contacted at all,
//     and NO flag clears it (contract: folder-trust). It is a config-FILE
//     contract, so no approval token can make a fresh-COPILOT_HOME Copilot
//     launch complete on its own. See docs/harnesses.md.
//
// Two things this catalog deliberately does NOT offer, both because the
// smallest honest catalog is the point:
//
//   - a `plan` token. `--mode plan` exists in Copilot's help output, but no
//     scenario measured its prompt behaviour, and an unmeasured "safe for
//     detached agents" claim is exactly what this ticket cannot afford.
//   - an --allow-all-paths / --allow-all / --yolo escalation. The path flags
//     WERE measured (out-of-cwd-paths), so such a token could have been
//     rendered honestly, but it is not needed for a nonblocking detached pane
//     and Copilot's built-in file edits are not OS-confined — outside a
//     tclaude-layer sandbox the path check is the only boundary on what the
//     agent writes. Adding it later needs a concrete user need and warning UX,
//     not merely the evidence that it works.
const (
	// CopilotApprovalInherit emits NO permission flags: the launch runs under
	// Copilot's own defaults plus whatever the operator's settings/config
	// persist. It is the faithful reconstruction of every Copilot launch
	// tclaude made before this catalog existed, which is why the daemon's
	// legacy-row fallback resolves to it rather than to the new default.
	CopilotApprovalInherit = "inherit"

	// CopilotApprovalAllowTools is the unattended default: tools run without
	// confirmation and the ask_user tool is removed, while directory access
	// stays precise — granted per directory from the resolved sandbox profile,
	// never wholesale.
	CopilotApprovalAllowTools = "allow-tools"
)

// copilotApprovalModes is the canonical ordered set shared by validation, the
// CLI/profile API and the dashboard selector. The default comes first so an
// empty legacy profile renders an explicit effective choice.
var copilotApprovalModes = []string{
	CopilotApprovalAllowTools, CopilotApprovalInherit,
}

type copilotApproval struct{}

func (copilotApproval) DefaultPolicy() string { return CopilotApprovalAllowTools }

func (copilotApproval) Modes() []string { return append([]string(nil), copilotApprovalModes...) }

func (copilotApproval) ValidatePolicy(policy string) (string, error) {
	policy = strings.TrimSpace(policy)
	if policy == "" {
		return policy, nil
	}
	if slices.Contains(copilotApprovalModes, policy) {
		return policy, nil
	}
	return "", fmt.Errorf("invalid copilot approval policy %q (want %s)",
		policy, strings.Join(copilotApprovalModes, "|"))
}

// The "⚠" prefixes the caveat half of the copy: the spawn UI collapses mode
// help behind a [?] but keeps everything from the ⚠ onward visible, so a mode
// that can strand a detached agent says so without the operator opening
// anything. Exactly one ⚠ per string, and it runs to the end.
var copilotApprovalModeHelp = map[string]string{
	CopilotApprovalAllowTools: "Run tools without confirmation and remove the ask_user tool. " +
		"Directory access is granted precisely, from the resolved sandbox profile, " +
		"and Copilot's own path check stays on. " +
		"⚠ Copilot has other prompt sources this does not close: a path outside every " +
		"granted directory still waits for a human, and a first launch in a folder " +
		"Copilot has not been told to trust blocks before the model is ever contacted.",
	CopilotApprovalInherit: "Emit no permission flags; the launch uses Copilot's own defaults and " +
		"whatever your Copilot configuration persists. " +
		"⚠ This is Copilot's prompting posture: a detached agent can block forever on tool " +
		"approval, on the ask_user tool, on URL access, or on directory access, and tclaude " +
		"cannot tell which from outside the pane.",
}

func (copilotApproval) ModeHelp(policy string) string {
	return copilotApprovalModeHelp[strings.TrimSpace(policy)]
}

// Flag spellings, taken verbatim from Copilot 1.0.77's option table and pinned
// here as constants because they are also what the copilotfixture scenarios
// launched with.
const (
	copilotFlagAllowAllTools = "--allow-all-tools"
	copilotFlagNoAskUser     = "--no-ask-user"
	copilotFlagAddDir        = "--add-dir"

	// copilotAllowAllEnv is the ambient promoter. --help presents it as the env
	// alias for --allow-all-tools, but it is measured STRICTLY STRONGER: with
	// no flags at all it also cleared the folder-trust gate that no flag
	// clears (contract: ambient-allow-all-env). An operator who exports it
	// turns every tclaude-spawned Copilot pane into an allow-all session with
	// no record anywhere, so the spawner unsets it. See copilotEnvScrub.
	copilotAllowAllEnv = "COPILOT_ALLOW_ALL"
)

// copilotPermissionArgs renders a validated policy into the exact argv
// fragments, in a stable order, so a spawn's recorded posture and its command
// line cannot disagree.
//
// addDirs are the resolved sandbox profile's additive roots. They are rendered
// INDEPENDENTLY of the token, because they are the path axis rather than the
// approval axis: the grants come from the sandbox profile either way, and
// Copilot's own directory check would otherwise prompt for a directory
// tclaude's outer sandbox already opened.
//
// An unrecognized policy renders nothing rather than guessing. Callers
// validate first (ResolveApprovalPolicy / ValidateApprovalPolicy), so this is
// a belt-and-braces arm, and rendering the default for an unknown token would
// silently promote a posture nobody selected.
func copilotPermissionArgs(policy string, addDirs []string) []string {
	var args []string
	switch strings.TrimSpace(policy) {
	case CopilotApprovalAllowTools:
		args = append(args, copilotFlagAllowAllTools, copilotFlagNoAskUser)
		args = append(args, copilotAddDirArgs(addDirs)...)
	case CopilotApprovalInherit:
		args = append(args, copilotAddDirArgs(addDirs)...)
	}
	return args
}

// copilotAddDirArgs renders one `--add-dir <dir>` per granted directory.
//
// Sorted and deduplicated so the same profile always produces the same argv:
// the launch command is recorded, compared across relaunches, and asserted on
// in tests, and a set whose iteration order leaked into it would make all
// three flaky. normalizedSandboxWriteDirs (shared with the Claude settings
// renderer) drops relative and empty entries — an --add-dir Copilot would
// resolve against a working directory tclaude only assumes.
func copilotAddDirArgs(dirs []string) []string {
	normalized := normalizedSandboxWriteDirs(dirs)
	if len(normalized) == 0 {
		return nil
	}
	sorted := append([]string(nil), normalized...)
	sort.Strings(sorted)
	args := make([]string, 0, len(sorted)*2)
	for _, dir := range sorted {
		args = append(args, copilotFlagAddDir, dir)
	}
	return args
}

// copilotSpawnAddDirs collects the directories a Copilot launch should be able
// to reach, from the same effective sandbox profile the outer sandbox is built
// from.
//
// Read and write roots are merged deliberately: Copilot's directory check is
// not read/write split — its dialog is a single "Allow directory access" — so
// modelling a distinction the CLI does not have would only invent one.
// SandboxDenyDirs are NOT rendered: --add-dir has no negative form, and
// silently turning a deny into an omission would be indistinguishable from
// never having had the rule.
func copilotSpawnAddDirs(spec SpawnSpec) []string {
	dirs := make([]string, 0, len(spec.SandboxReadDirs)+len(spec.SandboxWriteDirs))
	dirs = append(dirs, spec.SandboxReadDirs...)
	dirs = append(dirs, spec.SandboxWriteDirs...)
	return dirs
}
