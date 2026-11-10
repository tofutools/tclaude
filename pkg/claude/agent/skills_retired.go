package agent

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// retiredSkills maps a bundled skill's current name to the names it shipped
// under before. Installing the current skill removes a retired copy, so an
// agent is not offered both the old and the new name.
var retiredSkills = map[string][]string{
	"agent-drawing-board": {"agent-board"},
}

// removeRetiredSkills deletes the retired installs of name under root. A
// directory is removed only when its SKILL.md names itself as the retired
// skill: one the user wrote under that name stays.
func removeRetiredSkills(root, name string) error {
	for _, old := range retiredSkills[name] {
		dir := filepath.Join(root, old)
		if !isBundledSkillInstall(dir, old) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return nil
}

// isBundledSkillInstall reports whether dir holds a SKILL.md whose
// frontmatter says name: <name>.
func isBundledSkillInstall(dir, name string) bool {
	f, err := os.Open(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for i := 0; scanner.Scan() && i < 10; i++ {
		if strings.TrimSpace(scanner.Text()) == "name: "+name {
			return true
		}
	}
	return false
}
