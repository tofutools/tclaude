package agent

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// skillsFS holds the canonical skill files shipped with the binary. The
// CLI `tclaude setup --install-agent-skills` materialises them into
// ~/.claude/skills/ on demand, since `go install` strips the source tree
// and we can't symlink something that's no longer on disk.
//
//go:embed skills/agent-coord/SKILL.md skills/agent-rename/SKILL.md skills/agent-lifecycle/SKILL.md skills/agent-schedule/SKILL.md
var skillsFS embed.FS

// bundledSkills is the registry of skills shipped with tclaude. Add a new
// entry here (and a matching skills/<name>/ directory in the source tree)
// to ship another skill.
var bundledSkills = []string{
	"agent-coord",
	"agent-rename",
	"agent-lifecycle",
	"agent-schedule",
}

// InstalledSkill describes a skill that was written to disk.
type InstalledSkill struct {
	Name string // skill name (also the directory under ~/.claude/skills/)
	Path string // absolute path to the installed skill directory
}

// InstallSkills writes every bundled skill into ~/.claude/skills/<name>/.
// When force is false and a destination already exists, that single skill
// is skipped and ErrSkillExists is returned alongside whatever did install
// successfully.
func InstallSkills(force bool) ([]InstalledSkill, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("user home: %w", err)
	}

	var installed []InstalledSkill
	var firstExistsErr error
	for _, name := range bundledSkills {
		dst := filepath.Join(home, ".claude", "skills", name)
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
