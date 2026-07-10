package agent

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestInstallCodexSkillsInstallsBothUserRoots(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, "custom-codex")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)

	installed, err := InstallCodexSkills(true)

	require.NoError(t, err)
	assert.Len(t, installed, len(bundledSkills)*2)
	assert.DirExists(t, filepath.Join(home, ".agents", "skills", "agent-coord"))
	assert.DirExists(t, filepath.Join(codexHome, "skills", "agent-coord"))
}

func TestBundledSkillFrontmatterIsValidYAML(t *testing.T) {
	const maxCodexDescriptionChars = 1024

	type frontmatter struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}

	for _, name := range bundledSkills {
		t.Run(name, func(t *testing.T) {
			data, err := skillsFS.ReadFile("skills/" + name + "/SKILL.md")
			require.NoError(t, err)

			raw, err := skillFrontmatter(data)
			require.NoError(t, err)

			var got frontmatter
			require.NoError(t, yaml.Unmarshal([]byte(raw), &got))
			assert.Equal(t, name, got.Name)
			assert.NotEmpty(t, got.Description)
			assert.LessOrEqual(t, utf8.RuneCountInString(got.Description), maxCodexDescriptionChars)
		})
	}
}

// The bundled skills must document the spawn default-resolution chain and the
// policy-bound-spawn warning (TCL-304), so an agent that reads them before
// delegating knows an unset --harness can inherit a default profile's vendor.
func TestBundledSkillsDocumentSpawnResolution(t *testing.T) {
	coord, err := skillsFS.ReadFile("skills/agent-coord/SKILL.md")
	require.NoError(t, err)
	body := string(coord)
	assert.Contains(t, body, "group's default spawn profile")
	assert.Contains(t, body, "global (dashboard) default spawn profile")
	assert.Contains(t, body, "silently flip vendor")
	assert.Contains(t, body, "tclaude agent profiles default show")
	assert.Contains(t, body, "tclaude agent groups ls")

	circles, err := skillsFS.ReadFile("skills/agent-circles/SKILL.md")
	require.NoError(t, err)
	assert.Contains(t, string(circles), "carries its own `harness`")
}

func skillFrontmatter(data []byte) (string, error) {
	const marker = "---\n"
	text := string(data)
	if !strings.HasPrefix(text, marker) {
		return "", fmt.Errorf("missing opening frontmatter marker")
	}
	rest := strings.TrimPrefix(text, marker)
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", fmt.Errorf("missing closing frontmatter marker")
	}
	return rest[:end], nil
}
