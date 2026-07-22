package agentd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// sandboxGlobalFilesystemRuleJSON is one read-only filesystem row inherited
// from a harness-level config file. These rows are deliberately separate from
// sandboxProfileJSON.Filesystem: the dashboard may explain the effective
// launch context without accidentally persisting ambient config into a named
// profile.
type sandboxGlobalFilesystemRuleJSON struct {
	Path      string                                  `json:"path"`
	Access    string                                  `json:"access"`
	Harnesses []string                                `json:"harnesses"`
	Origins   []sandboxGlobalFilesystemRuleOriginJSON `json:"origins"`
}

type sandboxGlobalFilesystemRuleOriginJSON struct {
	Harness string `json:"harness"`
	Source  string `json:"source"`
	Setting string `json:"setting"`
	Note    string `json:"note,omitempty"`
}

type sandboxGlobalFilesystemRulesJSON struct {
	Filesystem []sandboxGlobalFilesystemRuleJSON `json:"filesystem"`
	Warnings   []string                          `json:"warnings"`
}

type sandboxGlobalFilesystemRuleCandidate struct {
	path   string
	access string
	origin sandboxGlobalFilesystemRuleOriginJSON
}

// sandboxGlobalFilesystemRules reads only the filesystem portions of the two
// harness configs that compose beneath a named sandbox profile:
//
//   - Claude Code's user settings.json sandbox block; and
//   - tclaude's managed Codex permission profile.
//
// A missing config is an empty layer, not an error. A malformed config is
// reported as an inline warning while the other harness remains useful. This
// feed is explanatory only and must never make the editor unable to save its
// own independent profile payload.
func sandboxGlobalFilesystemRules() sandboxGlobalFilesystemRulesJSON {
	home, err := os.UserHomeDir()
	if err != nil {
		return sandboxGlobalFilesystemRulesJSON{Warnings: []string{"Could not resolve the home directory used by global sandbox config."}}
	}

	candidates := make([]sandboxGlobalFilesystemRuleCandidate, 0)
	warnings := make([]string, 0)
	claudeRules, claudeWarning := readClaudeGlobalFilesystemRules(home)
	candidates = append(candidates, claudeRules...)
	if claudeWarning != "" {
		warnings = append(warnings, claudeWarning)
	}
	codexRules, codexWarning := readCodexGlobalFilesystemRules(home)
	candidates = append(candidates, codexRules...)
	if codexWarning != "" {
		warnings = append(warnings, codexWarning)
	}

	return sandboxGlobalFilesystemRulesJSON{
		Filesystem: mergeSandboxGlobalFilesystemRules(home, candidates),
		Warnings:   warnings,
	}
}

func readClaudeGlobalFilesystemRules(home string) ([]sandboxGlobalFilesystemRuleCandidate, string) {
	path := session.ClaudeSettingsPath()
	if path == "" {
		return nil, "Could not locate Claude Code's global settings.json."
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, ""
	}
	if err != nil {
		return nil, "Could not read Claude Code's global sandbox settings: " + err.Error()
	}
	var settings struct {
		Sandbox struct {
			Enabled    *bool `json:"enabled"`
			Filesystem struct {
				AllowRead  []string `json:"allowRead"`
				AllowWrite []string `json:"allowWrite"`
				DenyRead   []string `json:"denyRead"`
				DenyWrite  []string `json:"denyWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		// Parser diagnostics can include source excerpts. The settings file may
		// carry credentials outside the sandbox block, so never return the raw
		// diagnostic through an agent-readable endpoint.
		return nil, "Could not parse Claude Code's global sandbox settings: settings.json is not valid JSON."
	}

	note := "Applies when the Claude Code sandbox is enabled."
	if settings.Sandbox.Enabled != nil {
		if *settings.Sandbox.Enabled {
			note = "Claude Code's global sandbox is enabled."
		} else {
			note = "Configured globally, but inactive unless the launch forces the Claude Code sandbox on."
		}
	}
	source := displaySandboxGlobalPath(home, path)
	filesystem := settings.Sandbox.Filesystem
	candidates := make([]sandboxGlobalFilesystemRuleCandidate, 0,
		len(filesystem.AllowRead)+len(filesystem.AllowWrite)+len(filesystem.DenyRead)+len(filesystem.DenyWrite))
	appendRules := func(paths []string, access, setting string) {
		for _, rulePath := range paths {
			if strings.TrimSpace(rulePath) == "" {
				continue
			}
			candidates = append(candidates, sandboxGlobalFilesystemRuleCandidate{
				path: rulePath, access: access,
				origin: sandboxGlobalFilesystemRuleOriginJSON{
					Harness: "claude", Source: source, Setting: setting, Note: note,
				},
			})
		}
	}
	appendRules(filesystem.AllowRead, "read", "sandbox.filesystem.allowRead")
	appendRules(filesystem.AllowWrite, "write", "sandbox.filesystem.allowWrite")

	// A paired denyRead + denyWrite is the same read/write denial represented by
	// one named-profile `deny` row and one Codex `none` entry. Collapse only the
	// exact pair; one-sided Claude rules remain explicit below.
	denyRead := sandboxGlobalPathSet(home, filesystem.DenyRead)
	denyWrite := sandboxGlobalPathSet(home, filesystem.DenyWrite)
	for identity, rulePath := range denyRead {
		if _, ok := denyWrite[identity]; !ok {
			appendRules([]string{rulePath}, "deny-read", "sandbox.filesystem.denyRead")
			continue
		}
		appendRules([]string{rulePath}, "deny", "sandbox.filesystem.denyRead + denyWrite")
		delete(denyWrite, identity)
	}
	for _, rulePath := range denyWrite {
		appendRules([]string{rulePath}, "deny-write", "sandbox.filesystem.denyWrite")
	}
	return candidates, ""
}

func readCodexGlobalFilesystemRules(home string) ([]sandboxGlobalFilesystemRuleCandidate, string) {
	path, err := harness.CodexAgentProfilePath()
	if err != nil {
		return nil, "Could not locate the managed Codex sandbox profile: " + err.Error()
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, ""
	}
	if err != nil {
		return nil, "Could not read the managed Codex sandbox profile: " + err.Error()
	}
	var config struct {
		Permissions map[string]struct {
			Filesystem map[string]string `toml:"filesystem"`
		} `toml:"permissions"`
	}
	if err := toml.Unmarshal(data, &config); err != nil {
		return nil, "Could not parse the managed Codex sandbox profile: tclaude-agent.config.toml is not valid TOML."
	}
	profile, ok := config.Permissions[harness.CodexAgentProfile]
	if !ok {
		return nil, fmt.Sprintf("The managed Codex sandbox profile does not define permissions.%s.", harness.CodexAgentProfile)
	}
	source := displaySandboxGlobalPath(home, path)
	paths := make([]string, 0, len(profile.Filesystem))
	for rulePath := range profile.Filesystem {
		paths = append(paths, rulePath)
	}
	sort.Strings(paths)
	candidates := make([]sandboxGlobalFilesystemRuleCandidate, 0, len(paths))
	for _, rulePath := range paths {
		access := ""
		switch strings.TrimSpace(profile.Filesystem[rulePath]) {
		case "read":
			access = "read"
		case "write":
			access = "write"
		case "none":
			access = "deny"
		default:
			continue
		}
		candidates = append(candidates, sandboxGlobalFilesystemRuleCandidate{
			path: rulePath, access: access,
			origin: sandboxGlobalFilesystemRuleOriginJSON{
				Harness: "codex", Source: source,
				Setting: fmt.Sprintf("permissions.%s.filesystem", harness.CodexAgentProfile),
				Note:    "Applied by tclaude's managed Codex sandbox profile.",
			},
		})
	}
	return candidates, ""
}

func sandboxGlobalPathSet(home string, paths []string) map[string]string {
	out := make(map[string]string, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		out[sandboxGlobalPathIdentity(home, path)] = path
	}
	return out
}

func mergeSandboxGlobalFilesystemRules(home string, candidates []sandboxGlobalFilesystemRuleCandidate) []sandboxGlobalFilesystemRuleJSON {
	byKey := make(map[string]*sandboxGlobalFilesystemRuleJSON, len(candidates))
	for _, candidate := range candidates {
		identity := sandboxGlobalPathIdentity(home, candidate.path)
		key := identity + "\x00" + candidate.access
		rule := byKey[key]
		if rule == nil {
			rule = &sandboxGlobalFilesystemRuleJSON{
				Path: displaySandboxGlobalPath(home, identity), Access: candidate.access,
			}
			byKey[key] = rule
		}
		duplicate := false
		for _, origin := range rule.Origins {
			if origin == candidate.origin {
				duplicate = true
				break
			}
		}
		if !duplicate {
			rule.Origins = append(rule.Origins, candidate.origin)
		}
	}

	out := make([]sandboxGlobalFilesystemRuleJSON, 0, len(byKey))
	for _, rule := range byKey {
		harnessSet := map[string]bool{}
		for _, origin := range rule.Origins {
			harnessSet[origin.Harness] = true
		}
		for _, name := range []string{"claude", "codex"} {
			if harnessSet[name] {
				rule.Harnesses = append(rule.Harnesses, name)
			}
		}
		sort.Slice(rule.Origins, func(i, j int) bool {
			if rule.Origins[i].Harness != rule.Origins[j].Harness {
				return rule.Origins[i].Harness < rule.Origins[j].Harness
			}
			return rule.Origins[i].Setting < rule.Origins[j].Setting
		})
		out = append(out, *rule)
	}
	accessOrder := map[string]int{"deny": 0, "deny-read": 1, "deny-write": 2, "read": 3, "write": 4}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return accessOrder[out[i].Access] < accessOrder[out[j].Access]
	})
	return out
}

func sandboxGlobalPathIdentity(home, path string) string {
	path = strings.TrimSpace(path)
	if path == "~" {
		path = home
	} else if strings.HasPrefix(path, "~/") {
		path = filepath.Join(home, path[2:])
	}
	return filepath.Clean(path)
}

func displaySandboxGlobalPath(home, path string) string {
	path = filepath.Clean(path)
	home = filepath.Clean(home)
	if path == home {
		return "~"
	}
	if rel, err := filepath.Rel(home, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "~/" + filepath.ToSlash(rel)
	}
	return path
}
