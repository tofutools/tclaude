package sandboxpolicy

import (
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// The common-rule catalog is a presentation-layer convenience, NOT a policy
// mechanism (TCL-623). Each entry describes an audited set of host paths that
// operators commonly want denied, with the labels, descriptions and warnings a
// UI needs to explain the consequences. The dashboard uses it to INSERT
// ordinary deny rows into a profile's filesystem table; nothing about the
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
)

const (
	// CommonRuleTierPortable groups leaf denies that are safe to add on their
	// own: they remove a specific secret/cache location and need no reopens.
	CommonRuleTierPortable = "portable"
	// CommonRuleTierHome marks the whole-home deny, which is only usable in
	// combination with narrower reopens and needs a capable harness/mode.
	CommonRuleTierHome = "home"
)

// CommonRule is one preset in the catalog. Paths are resolved for display on
// the current machine and are never persisted as an ID: the UI inserts them as
// ordinary deny rows.
type CommonRule struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Warning     string   `json:"warning,omitempty"`
	Tier        string   `json:"tier"`
	Paths       []string `json:"paths"`
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
	return home, nil
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
	for i := range rules {
		// Use the same longest-existing-ancestor canonicalization as ordinary
		// grants, so an inserted row is byte-identical to what normalization
		// would produce for the same path.
		for j, path := range rules[i].Paths {
			resolved, _, err := canonicalDirectory(path, true)
			if err != nil {
				return nil, fmt.Errorf("canonicalize common rule %q path %q: %w", rules[i].ID, path, err)
			}
			rules[i].Paths[j] = resolved
		}
		sort.Strings(rules[i].Paths)
	}
	return rules, nil
}

// CurrentCommonRuleCatalog resolves the catalog for the running platform.
func CurrentCommonRuleCatalog(home string) ([]CommonRule, error) {
	return CommonRuleCatalog(home, runtime.GOOS)
}
