package sandboxpolicy

import (
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

const (
	ReadExclusionCatalogVersion = 1
	MaxReadBaselineExclusions   = 64

	ReadExclusionSSH             = "secrets.ssh"
	ReadExclusionGnuPG           = "secrets.gnupg"
	ReadExclusionCloud           = "secrets.cloud"
	ReadExclusionVCSTokens       = "secrets.vcs-tokens"
	ReadExclusionToolchainCaches = "toolchain.caches"
	ReadExclusionBrowserProfiles = "browser.profiles"
	ReadExclusionHome            = "home.directory"
)

const (
	ReadExclusionTierPortable = "portable"
	ReadExclusionTierHome     = "home"
)

// ReadExclusionCategory is one stable, semantic restriction from catalog v1.
// Paths are resolved for display/enforcement on the current machine and are
// never persisted in a sandbox profile or snapshot.
type ReadExclusionCategory struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Warning     string   `json:"warning,omitempty"`
	Tier        string   `json:"tier"`
	Paths       []string `json:"paths"`
}

var readExclusionIDRE = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$`)

// NormalizeReadBaselineExclusions validates the portable identifier shape but
// deliberately preserves IDs this binary does not know. A newer export must
// remain visible and fail closed at launch, never be silently weakened by an
// older importer. The canonical form is a sorted, duplicate-free slice.
func NormalizeReadBaselineExclusions(in []string) ([]string, error) {
	if len(in) > MaxReadBaselineExclusions {
		return nil, fmt.Errorf("read_baseline_exclusions has too many entries (maximum %d)", MaxReadBaselineExclusions)
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if !readExclusionIDRE.MatchString(id) {
			return nil, fmt.Errorf("read_baseline_exclusions ID %q is invalid", raw)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func canonicalCatalogHome(home string) (string, error) {
	home = filepath.Clean(strings.TrimSpace(home))
	if home == "." || !filepath.IsAbs(home) {
		return "", fmt.Errorf("home directory %q is not absolute", home)
	}
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = filepath.Clean(resolved)
	}
	return home, nil
}

// ReadExclusionCatalog returns catalog v1 resolved for one platform/home.
// The stable IDs and descriptions are portable; only Paths are host values.
func ReadExclusionCatalog(home, goos string) ([]ReadExclusionCategory, error) {
	home, err := canonicalCatalogHome(home)
	if err != nil {
		return nil, err
	}
	under := func(parts ...string) string {
		return filepath.Join(append([]string{home}, parts...)...)
	}
	categories := []ReadExclusionCategory{
		{ID: ReadExclusionSSH, Label: "Deny audited default SSH locations", Description: "Default ~/.ssh location for SSH private keys, host trust, and client configuration.", Tier: ReadExclusionTierPortable, Paths: []string{under(".ssh")}},
		{ID: ReadExclusionGnuPG, Label: "Deny audited default GnuPG locations", Description: "Default ~/.gnupg location for OpenPGP private keys, trust databases, and agent configuration.", Tier: ReadExclusionTierPortable, Paths: []string{under(".gnupg")}},
		{ID: ReadExclusionCloud, Label: "Deny audited default cloud/container locations", Description: "Default Home locations for AWS, Google Cloud, Azure, Kubernetes, and Docker client credentials/configuration.", Tier: ReadExclusionTierPortable, Paths: []string{under(".aws"), under(".config", "gcloud"), under(".azure"), under(".kube"), under(".docker")}},
		{ID: ReadExclusionVCSTokens, Label: "Deny audited default VCS CLI locations", Description: "Default Home locations for GitHub CLI and GitLab CLI authentication/configuration.", Tier: ReadExclusionTierPortable, Paths: []string{under(".config", "gh"), under(".config", "glab")}},
		{ID: ReadExclusionToolchainCaches, Label: "Deny audited default toolchain-cache locations", Description: "Default Home locations for package-manager, compiler, version-manager, and dependency caches.", Warning: "Denying toolchain caches can make builds fail or force downloads; grant an agent-owned cache when the task needs one.", Tier: ReadExclusionTierPortable, Paths: []string{under(".npm"), under(".cargo"), under(".rustup"), under("go", "pkg", "mod"), under(".m2"), under(".gradle"), under(".local", "share", "mise"), under(".nvm"), under(".pyenv")}},
	}
	browser := ReadExclusionCategory{ID: ReadExclusionBrowserProfiles, Label: "Deny audited default browser-profile locations", Description: "Default Chrome, Chromium, and Firefox profile locations, including cookies and saved sessions.", Tier: ReadExclusionTierPortable}
	switch strings.TrimSpace(goos) {
	case "linux":
		browser.Paths = []string{under(".config", "google-chrome"), under(".config", "chromium"), under(".mozilla", "firefox")}
	case "darwin":
		browser.Paths = []string{under("Library", "Application Support", "Google", "Chrome"), under("Library", "Application Support", "Chromium"), under("Library", "Application Support", "Firefox", "Profiles")}
	default:
		browser.Warning = "This platform has no audited browser-profile path mapping; launch fails closed while this category is selected."
	}
	categories = append(categories, browser, ReadExclusionCategory{
		ID: ReadExclusionHome, Label: "Deny access to the Home directory", Description: "Deny the broad home-directory baseline, then reopen only workspace, control-plane, runtime, agent-directory, and explicitly granted paths required by the launch contract.", Warning: "Requires Claude sandbox-on, or Codex on Linux after its split-policy bubblewrap behavior is verified. Codex macOS and legacy Landlock are refused.", Tier: ReadExclusionTierHome, Paths: []string{home},
	})
	for i := range categories {
		// Use the same longest-existing-ancestor canonicalization as ordinary
		// grants. A missing leaf beneath a symlinked parent must resolve through
		// that parent now; otherwise a conflicting grant can pass validation and
		// become a reopen when the leaf is created later.
		for j, path := range categories[i].Paths {
			resolved, _, err := canonicalDirectory(path, true)
			if err != nil {
				return nil, fmt.Errorf("canonicalize read exclusion %q path %q: %w", categories[i].ID, path, err)
			}
			categories[i].Paths[j] = resolved
		}
		sort.Strings(categories[i].Paths)
	}
	return categories, nil
}

func CurrentReadExclusionCatalog(home string) ([]ReadExclusionCategory, error) {
	return ReadExclusionCatalog(home, runtime.GOOS)
}

// ResolveReadBaselineExclusions resolves known IDs and returns unknown IDs
// separately. Callers must refuse a non-empty unknown result.
func ResolveReadBaselineExclusions(ids []string, home, goos string) (map[string]ReadExclusionCategory, []string, error) {
	normalized, err := NormalizeReadBaselineExclusions(ids)
	if err != nil {
		return nil, nil, err
	}
	catalog, err := ReadExclusionCatalog(home, goos)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]ReadExclusionCategory, len(catalog))
	for _, category := range catalog {
		byID[category.ID] = category
	}
	resolved := make(map[string]ReadExclusionCategory, len(normalized))
	var unknown []string
	for _, id := range normalized {
		category, ok := byID[id]
		if !ok || len(category.Paths) == 0 {
			unknown = append(unknown, id)
			continue
		}
		resolved[id] = category
	}
	return resolved, unknown, nil
}

// ReadExclusionDenyPathsForOS returns the deduplicated current-host deny set.
// Unknown IDs are returned for the harness capability boundary to reject.
func ReadExclusionDenyPathsForOS(ids []string, home, goos string) ([]string, []string, error) {
	resolved, unknown, err := ResolveReadBaselineExclusions(ids, home, goos)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	var paths []string
	for _, category := range resolved {
		for _, path := range category.Paths {
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	sort.Strings(paths)
	return paths, unknown, nil
}
