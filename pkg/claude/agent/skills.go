package agent

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/common/skillroots"
)

// skillsFS holds the canonical skill files shipped with the binary. The CLI
// `tclaude setup --install-agent-skills` materialises them into each supported
// agent harness's user skill directory on demand, since `go install` strips the
// source tree and we can't symlink something that's no longer on disk.
//
//go:embed skills/agent-coord/SKILL.md skills/agent-rename/SKILL.md skills/agent-task/SKILL.md skills/present-pr-to-operator/SKILL.md skills/agent-lifecycle/SKILL.md skills/reincarnate/SKILL.md skills/agent-schedule/SKILL.md skills/agent-remote-control/SKILL.md skills/agent-dir/SKILL.md skills/agent-circles/SKILL.md skills/human-notify/SKILL.md skills/human-clipboard/SKILL.md skills/proxy-git/SKILL.md skills/proxy-linear/SKILL.md skills/process-templates
var skillsFS embed.FS

// bundledSkills is the registry of generally useful skills shipped with
// tclaude. These are installed by `tclaude setup --install-agent-skills`.
var bundledSkills = []string{
	"agent-coord",
	"agent-rename",
	"agent-task",
	"present-pr-to-operator",
	"agent-lifecycle",
	"reincarnate",
	"agent-schedule",
	"agent-remote-control",
	"agent-dir",
	"agent-circles",
	"human-notify",
	"human-clipboard",
	"process-templates",
}

// bundledProxySkills are useful only when the operator has configured the
// corresponding credential proxy. Keep them out of the default agent-skill
// set so installing the ordinary coordination tools does not advertise
// unavailable proxy capabilities to every agent.
var bundledProxySkills = []string{
	"proxy-git",
	"proxy-linear",
}

const proxyOptInMarkerName = ".tclaude-explicit-proxy-opt-in"

// InstalledSkill describes a skill that was written to disk.
type InstalledSkill struct {
	Name string // skill name (also the install directory basename)
	Path string // absolute path to the installed skill directory
}

// InstallSkills writes every ordinary bundled skill into
// ~/.claude/skills/<name>/. Proxy skills have a separate explicit installer.
// When force is false and a destination already exists, that single skill
// is skipped and ErrSkillExists is returned alongside whatever did install
// successfully.
func InstallSkills(force bool) ([]InstalledSkill, error) {
	root, err := skillroots.Claude()
	if err != nil {
		return nil, err
	}
	return installSkillsInRoot(root, force, bundledSkills)
}

// InstallCodexSkills writes every ordinary bundled skill into Codex's
// user-scope skill directories. Codex's current public docs name
// ~/.agents/skills; current Codex CLI skill tooling installs into
// $CODEX_HOME/skills, defaulting to ~/.codex/skills. Install both so /skills
// sees the bundle across layouts.
func InstallCodexSkills(force bool) ([]InstalledSkill, error) {
	return installCodexSkills(force, bundledSkills)
}

// InstallProxySkills writes the optional proxy skills into
// ~/.claude/skills/<name>/.
func InstallProxySkills(force bool) ([]InstalledSkill, error) {
	root, err := skillroots.Claude()
	if err != nil {
		return nil, err
	}
	installed, err := installSkillsInRoot(root, force, bundledProxySkills)
	if err != nil {
		return installed, err
	}
	return installed, markProxySkillsOptedIn(installed)
}

// InstallCodexProxySkills writes the optional proxy skills into Codex's
// user-scope skill directories.
func InstallCodexProxySkills(force bool) ([]InstalledSkill, error) {
	installed, err := installCodexSkills(force, bundledProxySkills)
	if err != nil {
		return installed, err
	}
	return installed, markProxySkillsOptedIn(installed)
}

// RetireLegacyProxySkills disables proxy skills installed in Claude Code's
// user skill root by releases where they belonged to the ordinary bundle.
// Only an unmarked manifest that exactly matches the embedded copy is moved;
// explicit opt-ins and locally modified skills are preserved.
func RetireLegacyProxySkills() ([]InstalledSkill, error) {
	root, err := skillroots.Claude()
	if err != nil {
		return nil, err
	}
	return retireLegacyProxySkillsInRoot(root)
}

// RetireLegacyCodexProxySkills disables legacy bundled proxy skills in both
// Codex user skill roots.
func RetireLegacyCodexProxySkills() ([]InstalledSkill, error) {
	roots, err := codexSkillRoots()
	if err != nil {
		return nil, err
	}
	var retired []InstalledSkill
	for _, root := range roots {
		got, err := retireLegacyProxySkillsInRoot(root)
		retired = append(retired, got...)
		if err != nil {
			return retired, err
		}
	}
	return retired, nil
}

func installCodexSkills(force bool, skills []string) ([]InstalledSkill, error) {
	roots, err := codexSkillRoots()
	if err != nil {
		return nil, err
	}

	var installed []InstalledSkill
	var firstExistsErr error
	for _, root := range roots {
		got, err := installSkillsInRoot(root, force, skills)
		installed = append(installed, got...)
		if err == nil {
			continue
		}
		if errors.Is(err, ErrSkillExists) {
			if firstExistsErr == nil {
				firstExistsErr = ErrSkillExists
			}
			continue
		}
		return installed, err
	}
	if firstExistsErr != nil {
		return installed, firstExistsErr
	}
	return installed, nil
}

func installSkillsInRoot(root string, force bool, skills []string) ([]InstalledSkill, error) {
	var installed []InstalledSkill
	var firstExistsErr error
	for _, name := range skills {
		dst := filepath.Join(root, name)
		if !force {
			if _, err := os.Stat(dst); err == nil {
				if firstExistsErr == nil {
					firstExistsErr = ErrSkillExists
				}
				continue
			}
		}
		if err := writeSkillTree(name, dst); err != nil {
			return installed, err
		}
		installed = append(installed, InstalledSkill{Name: name, Path: dst})
	}
	if firstExistsErr != nil {
		return installed, firstExistsErr
	}
	return installed, nil
}

func markProxySkillsOptedIn(installed []InstalledSkill) error {
	for _, skill := range installed {
		marker := filepath.Join(skill.Path, proxyOptInMarkerName)
		if err := os.WriteFile(marker, []byte("installed by tclaude setup --install-proxy-skills\n"), 0o644); err != nil {
			return fmt.Errorf("mark proxy skill %s explicitly opted in: %w", skill.Path, err)
		}
	}
	return nil
}

func retireLegacyProxySkillsInRoot(root string) ([]InstalledSkill, error) {
	var retired []InstalledSkill
	for _, name := range bundledProxySkills {
		dir := filepath.Join(root, name)
		if _, err := os.Stat(filepath.Join(dir, proxyOptInMarkerName)); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return retired, fmt.Errorf("inspect proxy opt-in marker in %s: %w", dir, err)
		}

		manifest := filepath.Join(dir, "SKILL.md")
		got, err := os.ReadFile(manifest)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return retired, fmt.Errorf("read legacy proxy skill %s: %w", manifest, err)
		}
		want, err := skillsFS.ReadFile("skills/" + name + "/SKILL.md")
		if err != nil {
			return retired, fmt.Errorf("read embedded proxy skill %s: %w", name, err)
		}
		if !bytes.Equal(got, want) {
			continue
		}

		backup := manifest + ".disabled-by-tclaude"
		for suffix := 2; ; suffix++ {
			if _, err := os.Lstat(backup); errors.Is(err, os.ErrNotExist) {
				break
			} else if err != nil {
				return retired, fmt.Errorf("inspect retired proxy skill path %s: %w", backup, err)
			}
			backup = fmt.Sprintf("%s.disabled-by-tclaude.%d", manifest, suffix)
		}
		if err := os.Rename(manifest, backup); err != nil {
			return retired, fmt.Errorf("retire legacy proxy skill %s: %w", manifest, err)
		}
		retired = append(retired, InstalledSkill{Name: name, Path: backup})
	}
	return retired, nil
}

func codexSkillRoots() ([]string, error) {
	return skillroots.Codex()
}

// writeSkillTree copies the embedded skills/<name>/ subtree into dst.
func writeSkillTree(name, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}
	root := "skills/" + name
	return fs.WalkDir(skillsFS, root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := skillsFS.ReadFile(p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, data, 0o644)
	})
}

// ErrSkillExists is returned by InstallSkills when at least one
// destination directory already exists and force was not set. Whatever
// did install successfully is still returned alongside the error.
var ErrSkillExists = errSkillExists{}

type errSkillExists struct{}

func (errSkillExists) Error() string {
	return "at least one skill already installed; pass force=true to overwrite"
}
