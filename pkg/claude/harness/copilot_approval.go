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
//     needed its own deny rule). The committed contract could not reach
//     Copilot's OTHER URL consumer, the web_fetch tool, because the hermetic
//     lab runs under COPILOT_OFFLINE=true and that removes web_fetch from the
//     catalog entirely; a follow-up measurement against the same pinned binary,
//     with hermeticity kept by a rejecting capture proxy instead, establishes
//     that --allow-all-tools closes its URL dialog as well. So the token that
//     renders it is nonblocking for both URL consumers, and neither needs a
//     deny rule to keep a pane moving.
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
	sorted := copilotAddDirRoots(dirs)
	if len(sorted) == 0 {
		return nil
	}
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
// Read and write roots are merged, and the merge rests on an ASSUMPTION that
// this file should not be read as claiming is measured. The out-of-cwd-paths
// scenario exercised --add-dir against a READ (a `cat` outside every granted
// root); nothing establishes whether the grant also permits writes, and the
// only support for merging is that the dialog Copilot draws is a single "Allow
// directory access" with no read/write split in its wording.
//
// The merge is still the right default, because the alternative is worse in the
// direction that matters: modelling a read/write distinction the CLI may not
// have would mean withholding a write root the profile granted, and Copilot
// would then park the pane on a directory prompt for a path tclaude had already
// decided the agent may write. If the grant turns out to be read-only, the
// consequence is a prompt, not an escalation. If it turns out to imply write,
// every granted READ root is also writable to Copilot — which is exactly why
// ValidateCopilotAddDirGrants refuses a deny nested inside any rendered root
// rather than reasoning about which access it carries. Worth a fixture.
//
// SandboxDenyDirs are NOT rendered: --add-dir has no negative form, and
// silently turning a deny into an omission would be indistinguishable from
// never having had the rule. When a deny sits INSIDE a rendered root, omission
// is not even available — see ValidateCopilotAddDirGrants.
func copilotSpawnAddDirs(spec SpawnSpec) []string {
	dirs := make([]string, 0, len(spec.SandboxReadDirs)+len(spec.SandboxWriteDirs))
	dirs = append(dirs, spec.SandboxReadDirs...)
	dirs = append(dirs, spec.SandboxWriteDirs...)
	return dirs
}

// SandboxCopilotDenyInsideAddDir is the wire vocabulary for the refusal below.
const SandboxCopilotDenyInsideAddDir = "copilot-deny-inside-add-dir"

// ValidateCopilotAddDirGrants refuses a launch whose granted directory roots
// would pre-answer Copilot's directory dialog for a path the sandbox profile
// DENIES.
//
// The shape is the mirror image of ValidateSandboxReopenUnderDeny's: that one
// guards a grant nested under a deny, this one a deny nested under a grant. It
// needs its own gate because `--add-dir` has no negative form. Claude renders
// denyRead/denyWrite alongside its allows and lets the more specific rule win;
// Copilot's path check takes grants only, so a profile that reads `$HOME` and
// denies `$HOME/.ssh` collapses, on Copilot, to "read `$HOME`" — and the denied
// subtree stops prompting.
//
// The grant set is NOT just the flags tclaude renders. Copilot grants the
// cwd subtree and the system temp directory AUTOMATICALLY, with no flag
// (contract: out-of-cwd-paths), and those two are the common case by a wide
// margin: a profile that denies `<cwd>/.env` renders no `--add-dir` at all, so
// a gate that looked only at rendered roots would pass the launch and let the
// agent read and write the denied file through the implicit cwd grant. They are
// passed in rather than resolved here so the caller's own launch cwd and temp
// resolution stay the single source of truth.
//
// That is a real boundary loss rather than a theoretical one, and only outside
// tclaude-layer. The permission matrix records that Copilot's built-in file
// edits are not OS-confined, so for a launch with no outer wall the directory
// check IS the file boundary; silently widening it is exactly the escalation
// the catalog refuses to make with `--allow-all-paths`. Under tclaude-layer the
// outer wall enforces the deny whatever Copilot's own check believes, so the
// launch is admitted — the same reasoning the reopen-under-deny gate applies to
// the harnesses that can enforce it.
//
// It refuses rather than dropping the grant: SandboxReadDirs contracts that an
// adapter either renders its roots or rejects the launch, a dropped root would
// silently return the pane to prompting on a directory the operator granted,
// and the implicit roots cannot be dropped at all.
//
// KNOWN LIMIT: containment is byte-exact and lexical, so a differently-cased
// deny spelling on a case-insensitive volume, or a symlinked one (macOS TMPDIR
// through /var -> /private/var reaches this gate, since the temp root is one of
// the implicit grants), escapes it. That is the identity-only, guard-biased
// containment rule TCL-981 established and TCL-985 tracks converting the
// remaining sites to; this is one of them, and it is deliberately left to that
// pass rather than converted alone — pathContains is also used by a call site
// that is NOT a refusal guard, so the change belongs per-call-site.
func ValidateCopilotAddDirGrants(
	harnessName, cwd, tempDir string,
	readDirs, writeDirs, denyDirs []string,
	outerLayer bool,
) error {
	if strings.TrimSpace(harnessName) != CopilotName || outerLayer {
		return nil
	}
	denies := normalizedSandboxWriteDirs(denyDirs)
	if len(denies) == 0 {
		return nil
	}
	rendered := copilotAddDirRoots(append(append([]string(nil), readDirs...), writeDirs...))
	implicit := normalizedSandboxWriteDirs([]string{cwd, tempDir})
	for _, grant := range append(append([]string(nil), rendered...), implicit...) {
		for _, deny := range denies {
			if !pathContains(grant, deny) {
				continue
			}
			how := "granting the enclosing directory " + grant
			if slices.Contains(implicit, grant) && !slices.Contains(rendered, grant) {
				// Naming the mechanism matters here: nothing in the launch
				// command mentions this root, so an operator reading the argv
				// would have no idea where the grant came from.
				how = fmt.Sprintf(
					"Copilot grants the enclosing directory %s automatically, with no flag "+
						"(its launch directory and the system temp directory are always readable)", grant)
			}
			return &SandboxCapabilityError{
				Harness: CopilotName,
				Kind:    SandboxCopilotDenyInsideAddDir,
				Message: fmt.Sprintf(
					"this profile denies %s while %s, and Copilot's directory check takes grants "+
						"only — there is no counterpart to a deny, so the launch would open the "+
						"denied path to the agent. Copilot's built-in file edits are not "+
						"OS-confined, so outside --sandbox-impl tclaude-layer that check is the "+
						"only file boundary the launch has. Run this profile under "+
						"--sandbox-impl tclaude-layer, where the outer sandbox enforces the deny, "+
						"or move the denied path out of the granted directory",
					deny, how),
			}
		}
	}
	return nil
}

// copilotAddDirRoots returns the directories copilotAddDirArgs would render,
// without the flag tokens — so the gate above and the renderer cannot disagree
// about which roots a launch explicitly opens.
func copilotAddDirRoots(dirs []string) []string {
	normalized := normalizedSandboxWriteDirs(dirs)
	sorted := append([]string(nil), normalized...)
	sort.Strings(sorted)
	return sorted
}
