package agent

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/common/skillroots"
	utilityskills "github.com/tofutools/tclaude/skills"
)

// skillsFS holds the canonical skill files shipped with the binary. The CLI
// `tclaude setup --install-agent-skills` materialises them into each supported
// agent harness's user skill directory on demand, since `go install` strips the
// source tree and we can't symlink something that's no longer on disk.
//
//go:embed skills/agent-coord/SKILL.md skills/agent-rename/SKILL.md skills/agent-task/SKILL.md skills/present-pr-to-operator/SKILL.md skills/agent-lifecycle/SKILL.md skills/reincarnate/SKILL.md skills/agent-schedule/SKILL.md skills/agent-remote-control/SKILL.md skills/agent-dir/SKILL.md skills/agent-circles/SKILL.md skills/human-notify/SKILL.md skills/human-clipboard/SKILL.md skills/proxy-git/SKILL.md skills/proxy-linear/SKILL.md skills/proxy-awb/SKILL.md skills/proxy-http/SKILL.md skills/process-templates
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
var bundledProxySkillSpecs = []struct {
	name    string
	enabled func(ProxySkills) bool
}{
	{"proxy-git", func(s ProxySkills) bool { return s.Git }},
	{"proxy-linear", func(s ProxySkills) bool { return s.Linear }},
	{"proxy-awb", func(s ProxySkills) bool { return s.AWB }},
	{"proxy-http", func(s ProxySkills) bool { return s.HTTP }},
}

var bundledProxySkills = func() []string {
	skills := make([]string, 0, len(bundledProxySkillSpecs))
	for _, spec := range bundledProxySkillSpecs {
		skills = append(skills, spec.name)
	}
	return skills
}()

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

// InstallProxySkills writes the selected optional proxy skills into
// ~/.claude/skills/<name>/. An empty selection is a successful no-op.
func InstallProxySkills(force bool, selection ProxySkills) ([]InstalledSkill, error) {
	skills := selection.names()
	if len(skills) == 0 {
		return nil, nil
	}
	root, err := skillroots.Claude()
	if err != nil {
		return nil, err
	}
	return installSkillsInRoot(root, force, skills)
}

// InstallCodexProxySkills writes the selected optional proxy skills into
// Codex's user-scope skill directories. An empty selection is a successful
// no-op.
func InstallCodexProxySkills(force bool, selection ProxySkills) ([]InstalledSkill, error) {
	skills := selection.names()
	if len(skills) == 0 {
		return nil, nil
	}
	return installCodexSkills(force, skills)
}

// writeUtilitySkillTree copies one optional utility skill from the repo's
// skills/ package into dst.
func writeUtilitySkillTree(name, dst string) error {
	return writeSkillTreeFS(utilityskills.FS, name, dst)
}

// InstallUtilitySkills writes the optional utility skills (repo skills/, e.g.
// demo-recording) into ~/.claude/skills/<name>/. They are opt-in via
// `tclaude setup --install-utility-skills` and not part of --install-all.
func InstallUtilitySkills(force bool) ([]InstalledSkill, error) {
	root, err := skillroots.Claude()
	if err != nil {
		return nil, err
	}
	return installTreesInRoot(root, force, utilityskills.Names, writeUtilitySkillTree)
}

// InstallCodexUtilitySkills writes the optional utility skills into Codex's
// user-scope skill directories.
func InstallCodexUtilitySkills(force bool) ([]InstalledSkill, error) {
	return installCodexTrees(force, utilityskills.Names, writeUtilitySkillTree)
}

// ProxySkills selects which credential-proxy skills to install.
type ProxySkills struct {
	Git    bool
	Linear bool
	AWB    bool
	HTTP   bool
}

func (s ProxySkills) names() []string {
	var skills []string
	for _, spec := range bundledProxySkillSpecs {
		if spec.enabled(s) {
			skills = append(skills, spec.name)
		}
	}
	return skills
}

func installCodexSkills(force bool, skills []string) ([]InstalledSkill, error) {
	return installCodexTrees(force, skills, writeSkillTree)
}

func installCodexTrees(force bool, skills []string, write func(name, dst string) error) ([]InstalledSkill, error) {
	roots, err := codexSkillRoots()
	if err != nil {
		return nil, err
	}

	var installed []InstalledSkill
	var firstExistsErr error
	for _, root := range roots {
		got, err := installTreesInRoot(root, force, skills, write)
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
	return installTreesInRoot(root, force, skills, writeSkillTree)
}

func installTreesInRoot(root string, force bool, skills []string, write func(name, dst string) error) ([]InstalledSkill, error) {
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
		if err := write(name, dst); err != nil {
			return installed, err
		}
		installed = append(installed, InstalledSkill{Name: name, Path: dst})
	}
	if firstExistsErr != nil {
		return installed, firstExistsErr
	}
	return installed, nil
}

func codexSkillRoots() ([]string, error) {
	return skillroots.Codex()
}

// writeSkillTree copies the embedded skills/<name>/ subtree into dst.
func writeSkillTree(name, dst string) error {
	return writeSkillTreeFS(skillsFS, "skills/"+name, dst)
}

// writeSkillTreeFS copies the root subtree of fsys into dst.
func writeSkillTreeFS(fsys fs.FS, root, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}
	return fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, walkErr error) error {
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
		data, err := fs.ReadFile(fsys, p)
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
