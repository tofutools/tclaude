package agent

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// skillsFS holds the canonical skill files shipped with the binary. The
// CLI `tclaude setup --install-agent-skill` materialises them into
// ~/.claude/skills/ on demand, since `go install` strips the source tree
// and we can't symlink something that's no longer on disk.
//
//go:embed skills/agent-coord/SKILL.md
var skillsFS embed.FS

// SkillName is the directory name we install under ~/.claude/skills/.
const SkillName = "agent-coord"

// InstallSkill writes the bundled agent-coord skill to
// ~/.claude/skills/agent-coord/. If a skill of the same name already
// exists, it's overwritten only when force is true; otherwise the call
// returns ErrSkillExists so the caller can prompt.
func InstallSkill(force bool) (path string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	dst := filepath.Join(home, ".claude", "skills", SkillName)

	if !force {
		if _, err := os.Stat(dst); err == nil {
			return dst, ErrSkillExists
		}
	}

	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dst, err)
	}

	// Walk the embedded skills/<name>/ tree and copy each file into dst.
	root := "skills/" + SkillName
	err = fs.WalkDir(skillsFS, root, func(p string, d fs.DirEntry, walkErr error) error {
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
	if err != nil {
		return "", fmt.Errorf("install skill: %w", err)
	}
	return dst, nil
}

// ErrSkillExists is returned by InstallSkill when ~/.claude/skills/<name>
// already exists and force was not set. The caller decides whether to
// prompt or back off.
var ErrSkillExists = errSkillExists{}

type errSkillExists struct{}

func (errSkillExists) Error() string {
	return "skill already installed; pass force=true to overwrite"
}
