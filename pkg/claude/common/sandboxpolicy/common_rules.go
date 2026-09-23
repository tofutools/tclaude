package sandboxpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// The common-rule catalog is a presentation-layer convenience, NOT a policy
// mechanism (TCL-623). Each entry describes an audited set of host paths that
// operators commonly want denied — or, for the harness-state entries, granted
// writable — with the labels, descriptions and warnings a UI needs to explain
// the consequences. The dashboard uses it to INSERT ordinary rows into a
// profile's filesystem table; nothing about the
// chosen entry is persisted afterwards, and after insertion the rows are
// indistinguishable from hand-authored ones. That is the point: there is one
// mechanism (the filesystem table), and no hidden state claiming enforcement
// this binary does not perform.

const (
	CommonRuleCatalogVersion = 1

	CommonRuleSSH             = "secrets.ssh"
	CommonRuleGnuPG           = "secrets.gnupg"
	CommonRuleCloud           = "secrets.cloud"
	CommonRuleVCSTokens       = "secrets.vcs-tokens"
	CommonRuleToolchainCaches = "toolchain.caches"
	CommonRuleBrowserProfiles = "browser.profiles"
	CommonRuleHome            = "home.directory"

	CommonRuleClaudeState   = "harness.claude-state"
	CommonRuleCodexState    = "harness.codex-state"
	CommonRuleCopilotState  = "harness.copilot-state"
	CommonRuleOpenCodeState = "harness.opencode-state"
)

const (
	// CommonRuleTierPortable groups leaf denies that are safe to add on their
	// own: they remove a specific secret/cache location and need no reopens.
	CommonRuleTierPortable = "portable"
	// CommonRuleTierHome marks the whole-home deny, which is only usable in
	// combination with narrower reopens and needs a capable harness/mode.
	CommonRuleTierHome = "home"
	// CommonRuleTierHarness groups write grants for another harness's state,
	// which an agent needs to run that harness with `tclaude run`. The
	// executables themselves are exposed by every tclaude-layer launch.
	CommonRuleTierHarness = "harness"
)

// CommonRule is one preset in the catalog. Paths are resolved for display on
// the current machine and are never persisted as an ID: the UI inserts them as
// ordinary rows with the entry's Access, which is deny unless stated.
type CommonRule struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Warning     string `json:"warning,omitempty"`
	Tier        string `json:"tier"`
	// Access is the access of the inserted rows. Omitted means deny, which
	// keeps older dashboard builds, which always insert deny rows, correct for
	// every entry that omits it.
	Access Access   `json:"access,omitempty"`
	Paths  []string `json:"paths"`
	// ReadOnly are inserted as read rows beneath Paths. A harness-state
	// preset uses them to keep that harness's policy and code-execution
	// surface (its settings, hooks, skills, …) read-only while the state
	// around it is writable: those files take effect in the human's next
	// unsandboxed session of that harness, and the per-launch config floor
	// protects only the launched harness's own state.
	ReadOnly []string `json:"read_only,omitempty"`
	// Harness and StateRoot identify a harness-state preset so the caller
	// can fill ReadOnly from that harness's config-floor catalog.
	Harness   string `json:"harness,omitempty"`
	StateRoot string `json:"-"`
}

// CanonicalCommonRuleHome returns the home spelling used as the root of every
// common-rule catalog path. Callers that expose the catalog root alongside the
// rules must use this value so aliases compare against the same identity.
func CanonicalCommonRuleHome(home string) (string, error) {
	home = filepath.Clean(strings.TrimSpace(home))
	if home == "." || !filepath.IsAbs(home) {
		return "", fmt.Errorf("home directory %q is not absolute", home)
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = filepath.Clean(resolved)
	}
	// Every catalog rule path is built by joining onto this home, so restoring
	// its on-disk spelling here is what keeps those paths comparable with the
	// grants and protected roots they are evaluated against on a
	// case-insensitive volume.
	return CanonicalHostSpelling(home), nil
}

// CommonRuleCatalog returns catalog v1 resolved for one platform/home.
func CommonRuleCatalog(home, goos string) ([]CommonRule, error) {
	home, err := CanonicalCommonRuleHome(home)
	if err != nil {
		return nil, err
	}
	under := func(parts ...string) string {
		return filepath.Join(append([]string{home}, parts...)...)
	}
	rules := []CommonRule{
		{ID: CommonRuleSSH, Label: "Deny audited default SSH locations", Description: "Default ~/.ssh location for SSH private keys, host trust, and client configuration.", Tier: CommonRuleTierPortable, Paths: []string{under(".ssh")}},
		{ID: CommonRuleGnuPG, Label: "Deny audited default GnuPG locations", Description: "Default ~/.gnupg location for OpenPGP private keys, trust databases, and agent configuration.", Tier: CommonRuleTierPortable, Paths: []string{under(".gnupg")}},
		{ID: CommonRuleCloud, Label: "Deny audited default cloud/container locations", Description: "Default Home locations for AWS, Google Cloud, Azure, Kubernetes, and Docker client credentials/configuration.", Tier: CommonRuleTierPortable, Paths: []string{under(".aws"), under(".config", "gcloud"), under(".azure"), under(".kube"), under(".docker")}},
		{ID: CommonRuleVCSTokens, Label: "Deny audited default VCS CLI locations", Description: "Default Home locations for GitHub CLI and GitLab CLI authentication/configuration.", Tier: CommonRuleTierPortable, Paths: []string{under(".config", "gh"), under(".config", "glab")}},
		{ID: CommonRuleToolchainCaches, Label: "Deny audited default toolchain-cache locations", Description: "Default Home locations for package-manager, compiler, version-manager, and dependency caches.", Warning: "Denying toolchain caches can make builds fail or force downloads; grant an agent-owned cache when the task needs one.", Tier: CommonRuleTierPortable, Paths: []string{under(".npm"), under(".cargo"), under(".rustup"), under("go", "pkg", "mod"), under(".m2"), under(".gradle"), under(".local", "share", "mise"), under(".nvm"), under(".pyenv")}},
	}
	browser := CommonRule{ID: CommonRuleBrowserProfiles, Label: "Deny audited default browser-profile locations", Description: "Default Chrome, Chromium, and Firefox profile locations, including cookies and saved sessions.", Tier: CommonRuleTierPortable}
	switch strings.TrimSpace(goos) {
	case "linux":
		browser.Paths = []string{under(".config", "google-chrome"), under(".config", "chromium"), under(".mozilla", "firefox")}
	case "darwin":
		browser.Paths = []string{under("Library", "Application Support", "Google", "Chrome"), under("Library", "Application Support", "Chromium"), under("Library", "Application Support", "Firefox", "Profiles")}
	default:
		browser.Warning = "This platform has no audited browser-profile path mapping, so this rule inserts no rows here."
	}
	rules = append(rules, browser, CommonRule{
		ID: CommonRuleHome, Label: "Deny access to the Home directory", Description: "Deny the whole home directory, then reopen only the workspace and the specific paths the agent needs as ordinary read/write rows.",
		Warning: "Pair this with read/write rows for the toolchain dirs your agent needs (~/go, ~/.cargo, ~/.codex, …); tclaude reopens the workspace, Git admin paths and agent directories for you. Because those reopens sit beneath this deny, the launch requires Claude sandbox-on, or Codex on Linux with its split-policy backend verified; Codex macOS and legacy Landlock are refused.",
		Tier:    CommonRuleTierHome, Paths: []string{home},
	})
	harnessState := func(id, harnessName, name, stateRoot, description string, paths ...string) CommonRule {
		return CommonRule{
			ID: id, Label: "Allow " + name + " state for tclaude run", Description: description,
			Warning: "Grants write access to " + name + "'s login and session history. Its settings, hooks, skills and similar code surfaces are added as read-only rows; keep them, because those files run in your next unsandboxed " + name + " session.",
			Tier:    CommonRuleTierHarness, Access: AccessWrite, Paths: paths,
			Harness: harnessName, StateRoot: stateRoot,
		}
	}
	claude := harnessState(CommonRuleClaudeState, "claude", "Claude Code", under(".claude"), "The entries of Claude Code's ~/.claude state directory and its ~/.claude.json file, so an agent can run `tclaude run --harness claude`. ~/.claude/sessions is tclaude-protected and never granted, so ~/.claude itself cannot be; Claude Code runs without it. Entries Claude Code creates later need their own row.", claudeStatePaths(home)...)
	claude.Warning += " ~/.claude.json can declare MCP server commands and must stay writable for Claude Code, so it remains a residual hole."
	opencode := harnessState(CommonRuleOpenCodeState, "opencode", "OpenCode", "", "OpenCode's default XDG data, state and cache directories, with its config directory read-only, so an agent can run `tclaude run --harness opencode`.", under(".local", "share", "opencode"), under(".local", "state", "opencode"), under(".cache", "opencode"))
	opencode.ReadOnly = []string{under(".config", "opencode")}
	rules = append(rules,
		claude,
		harnessState(CommonRuleCodexState, "codex", "Codex", under(".codex"), "Codex's ~/.codex home (login and sessions), so an agent can run `tclaude run --harness codex`. A custom CODEX_HOME is not covered.", under(".codex")),
		harnessState(CommonRuleCopilotState, "copilot", "Copilot", under(".copilot"), "GitHub Copilot CLI's ~/.copilot home, so an agent can run `tclaude run --harness copilot`. A custom COPILOT_HOME is not covered.", under(".copilot")),
		opencode,
	)
	// Grant presets must never insert a row that saving would refuse.
	protected, _ := protectedPaths()
	for i := range rules {
		// Use the same longest-existing-ancestor canonicalization as ordinary
		// grants, so an inserted row is byte-identical to what normalization
		// would produce for the same path. Only a read/write row may name a
		// single file; a deny must name a directory.
		for j, path := range rules[i].Paths {
			rules[i].Paths[j] = ""
			var resolved string
			var err error
			if rules[i].Access == AccessWrite || rules[i].Access == AccessRead {
				resolved, _, _, err = canonicalGrantTarget(path, true)
				if err != nil || intersectsAny(resolved, protected) {
					// A grant preset is a convenience: an entry that cannot be
					// a rule (a socket, a protected path) is left out, not fatal.
					continue
				}
			} else {
				resolved, _, err = canonicalDirectory(path, true)
			}
			if err != nil {
				return nil, fmt.Errorf("canonicalize common rule %q path %q: %w", rules[i].ID, path, err)
			}
			rules[i].Paths[j] = resolved
		}
		for j, path := range rules[i].ReadOnly {
			if resolved, _, _, err := canonicalGrantTarget(path, true); err == nil {
				rules[i].ReadOnly[j] = resolved
			}
		}
		kept := rules[i].Paths[:0]
		for _, path := range rules[i].Paths {
			if path != "" {
				kept = append(kept, path)
			}
		}
		rules[i].Paths = kept
		sort.Strings(rules[i].Paths)
	}
	return rules, nil
}

// CurrentCommonRuleCatalog resolves the catalog for the running platform.
func CurrentCommonRuleCatalog(home string) ([]CommonRule, error) {
	return CommonRuleCatalog(home, runtime.GOOS)
}

// claudeStatePaths lists what the Claude Code state preset grants: every
// existing entry of ~/.claude except the tclaude-protected sessions directory
// (which no rule may reach, so ~/.claude itself cannot be granted), and the
// ~/.claude.json settings file.
func claudeStatePaths(home string) []string {
	dir := filepath.Join(home, ".claude")
	paths := []string{filepath.Join(home, ".claude.json")}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return paths
	}
	for _, entry := range entries {
		if entry.Name() == "sessions" {
			continue
		}
		paths = append(paths, filepath.Join(dir, entry.Name()))
	}
	return paths
}

func intersectsAny(path string, roots []string) bool {
	for _, root := range roots {
		if GuardPathsIntersect(path, root) {
			return true
		}
	}
	return false
}
