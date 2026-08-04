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
//     needed its own deny rule).
//   - URL access from the WEB_FETCH tool — the other URL consumer, and closed
//     by the same flag (contract: web-fetch-url-access). Worth stating in full
//     because the conservative posture that preceded it was right: web_fetch
//     was measured to be a THIRD independent deadlock source, alongside folder
//     trust and shell tool approval, so a detached pane really would have
//     parked there. --allow-all-tools was measured to close it ALONE, so the
//     result cannot be credited to --no-ask-user riding along beside it.
//     Neither URL consumer needs a deny rule to keep a pane moving.
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
//
// The load-bearing path caveat below is measured rather than assumed: the
// contract entry `path-dialog-under-allow-all-tools` records a real-PTY launch
// where an out-of-grant path stayed blocked, with the directory dialog naming
// its target, even while --allow-all-tools was present. Precise --add-dir grants
// therefore remain necessary under the unattended token.
var copilotApprovalModeHelp = map[string]string{
	CopilotApprovalAllowTools: "Run tools without confirmation and remove the ask_user tool. " +
		"Directory access is granted precisely, from the resolved sandbox profile, " +
		"rather than with a blanket path flag. " +
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
// A BLANK policy is not an unknown one, and conflating the two dropped the
// grants on the most ordinary launch there is. `tclaude session new --harness
// copilot` with no --ask-for-approval leaves the spec's policy empty on
// purpose: a human at a terminal is the trust root, so ValidateApprovalPolicy
// does not force a posture on them. What the SESSION ROW records for that same
// launch is `inherit`, and every reconstruction path — the daemon's
// approvalForHarness, resumeApprovalState, the relaunch profile — maps blank to
// `inherit` too. So blank already MEANS inherit everywhere else in tclaude, and
// rendering it as anything else here made one conversation's fresh launch and
// its own resume disagree: the resume rendered the profile's directories and
// the launch that created it did not. It also made ValidateCopilotAddDirGrants
// refuse a launch while naming an `--add-dir` root that launch never emitted.
// Blank therefore renders the INHERIT-shaped arm — the directory grants and
// nothing else. It must not render --allow-all-tools: not forcing a posture on
// a human is the entire reason the policy is blank.
//
// An unrecognized policy still renders nothing rather than guessing. Callers
// validate first, so that arm is belt-and-braces, and rendering the default for
// an unknown token would silently promote a posture nobody selected.
func copilotPermissionArgs(policy string, addDirs []string) []string {
	var args []string
	switch strings.TrimSpace(policy) {
	case CopilotApprovalAllowTools:
		args = append(args, copilotFlagAllowAllTools, copilotFlagNoAskUser)
		args = append(args, copilotAddDirArgs(addDirs)...)
	case CopilotApprovalInherit, "":
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
// Read and write roots are merged, and the contract entry `add-dir-write-grant`
// measures why: a real-PTY --add-dir launch wrote a fresh file and the fixture
// read back its exact content, while the no-grant sibling remained blocked on
// the directory dialog.
//
// The measured result makes the merge the right default: withholding a write
// root the profile granted would park the pane on a directory prompt for a
// path tclaude had already decided the agent may write. Every granted READ root
// is also writable to Copilot — which is exactly why ValidateCopilotAddDirGrants
// refuses a deny nested inside any rendered root rather than reasoning about
// which access it carries.
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
// The two sides of the containment test arrive spelled DIFFERENTLY, and that
// is why every path here goes through resolveSymlinks first. The denies come
// from a resolved sandbox profile, and sandboxpolicy.Resolve already walks each
// grant's symlinks — so a deny records its real target path. The implicit roots
// come raw from the launch: cwd as the operator's shell spells it, and tempDir
// straight out of TMPDIR. On macOS those two are never the same string, because
// /var is a symlink to /private/var and the system TMPDIR sits under it: the
// deny records /private/var/folders/…/secret while the temp root reads back as
// /var/folders/…, so a byte-exact comparison finds no containment and the gate
// passes a launch it exists to refuse. That is not a rare spelling — it is the
// macOS default, and the temp root is one of the two grants Copilot makes with
// no flag, so it would have been the most common way to reach this gate at all.
//
// KNOWN LIMIT: what remains is still a lexical containment rule over normalized
// spellings, not the identity-only, guard-biased rule TCL-981 established.
// TCL-985 tracks converting the remaining sites to GuardContainsOrEqual, and
// this is one of them; the normalization here fixes an ordinary disagreement
// between two tclaude-side producers rather than doing that conversion early.
// This is a change of ANSWER, not only of coverage, and the direction is not
// uniformly "refuses more". Resolving the deny side can also move a deny OUT of
// a root that lexically contained its authored spelling — a deny authored under
// a symlinked path whose target lies elsewhere. That is the correct answer:
// containment by real path identity is what decides whether the agent can reach
// the file, and the authored spelling was never the thing being granted. But it
// means the gate is not a strict superset of its previous self, and claiming
// otherwise would be exactly the kind of comfortable overstatement this catalog
// exists to avoid.
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
	for _, grant := range copilotGrantRoots(rendered, cwd, tempDir) {
		for _, deny := range denies {
			if !pathContains(grant.resolved, resolveSymlinks(deny)) {
				continue
			}
			// The message names the AUTHORED spellings throughout. An operator
			// reading a refusal needs to find these paths in their own profile
			// and their own environment, not in a resolved form neither one
			// contains.
			how := "granting the enclosing directory " + grant.authored
			if grant.implicit {
				// Naming the mechanism matters here: nothing in the launch
				// command mentions this root, so an operator reading the argv
				// would have no idea where the grant came from.
				how = fmt.Sprintf(
					"Copilot grants the enclosing directory %s automatically, with no flag "+
						"(its launch directory and the system temp directory are always readable)",
					grant.authored)
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

// copilotGrantRoot pairs a root's authored spelling, which the refusal message
// quotes, with the resolved spelling the containment test uses.
type copilotGrantRoot struct {
	authored string
	resolved string
	implicit bool
}

// copilotGrantRoots returns every directory the launch opens: the roots
// `--add-dir` names explicitly, then the cwd and temp roots Copilot opens with
// no flag. A root that is both keeps its rendered identity, so the refusal
// points at the flag an operator can actually see and change.
func copilotGrantRoots(rendered []string, cwd, tempDir string) []copilotGrantRoot {
	implicit := normalizedSandboxWriteDirs([]string{cwd, tempDir})
	roots := make([]copilotGrantRoot, 0, len(rendered)+len(implicit))
	for _, dir := range rendered {
		roots = append(roots, copilotGrantRoot{authored: dir, resolved: resolveSymlinks(dir)})
	}
	for _, dir := range implicit {
		if slices.Contains(rendered, dir) {
			continue
		}
		roots = append(roots, copilotGrantRoot{
			authored: dir, resolved: resolveSymlinks(dir), implicit: true,
		})
	}
	return roots
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
