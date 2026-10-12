package managedinstructions

import (
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/skillbundle"
	"gopkg.in/yaml.v3"
)

// RequiresCodeApproval includes hook-capable skill frontmatter, not just files
// whose name or mode identifies a script. Ambiguous metadata fails closed.
func RequiresCodeApproval(file skillbundle.File) bool {
	if skillbundle.SecurityRelevant(file) {
		return true
	}
	if file.Path != "SKILL.md" {
		return false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(string(file.Data), "\ufeff"))
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return false
	}
	end := 0
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" || strings.TrimSpace(lines[i]) == "..." {
			end = i
			break
		}
	}
	if end == 0 {
		return true
	}
	var metadata map[string]any
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &metadata); err != nil {
		return true
	}
	for key := range metadata {
		if strings.EqualFold(strings.TrimSpace(key), "hooks") {
			return true
		}
	}
	return false
}
