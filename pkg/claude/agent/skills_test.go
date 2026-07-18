package agent

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
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

// The bundled skills must document the spawn default-resolution chain, the
// policy-bound-spawn warning (TCL-304), and caller-dependent approval narrowing
// (TCL-585), so an agent can predict the launch it is delegating.
func TestBundledSkillsDocumentSpawnResolution(t *testing.T) {
	coord, err := skillsFS.ReadFile("skills/agent-coord/SKILL.md")
	require.NoError(t, err)
	body := string(coord)
	assert.Contains(t, body, "group's default spawn profile")
	assert.Contains(t, body, "global (dashboard) default spawn profile")
	assert.Contains(t, body, "silently flip vendor")
	assert.Contains(t, body, "resolved through that full chain first")
	assert.Contains(t, body, "incompatible explicit flag is a loud error")
	assert.Contains(t, body, "echo discloses the skip")
	assert.Contains(t, body, "tclaude agent profiles default show")
	assert.Contains(t, body, "tclaude agent groups ls")
	assert.Contains(t, body, "narrowed to caller posture")
	assert.Contains(t, body, "never silently reduced")

	circles, err := skillsFS.ReadFile("skills/agent-circles/SKILL.md")
	require.NoError(t, err)
	assert.Contains(t, string(circles), "carries its own `harness`")
}

func TestBundledLifecycleSkillsDocumentHarnessSpecificReincarnationPolicy(t *testing.T) {
	for _, name := range []string{"agent-coord", "agent-lifecycle", "reincarnate"} {
		t.Run(name, func(t *testing.T) {
			data, err := skillsFS.ReadFile("skills/" + name + "/SKILL.md")
			require.NoError(t, err)
			body := strings.Join(strings.Fields(string(data)), " ")
			assert.Contains(t, body, "automatic compaction")
			assert.Contains(t, body, "Do not reincarnate a Codex agent merely to free context space")
		})
	}

	lifecycle, err := skillsFS.ReadFile("skills/agent-lifecycle/SKILL.md")
	require.NoError(t, err)
	normalized := strings.Join(strings.Fields(string(lifecycle)), " ")
	assert.Contains(t, normalized, "The same harness policy applies to a target agent")
	assert.Contains(t, normalized, "Let Codex workers reach full context and auto-compact")
}

func TestProcessTemplateSkillPinsSafeAuthoringContract(t *testing.T) {
	data, err := skillsFS.ReadFile("skills/process-templates/SKILL.md")
	require.NoError(t, err)
	body := string(data)

	for _, required := range []string{
		"process.templates.read",
		"process.templates.manage",
		"--expect-source-hash",
		"process_template_conflict",
		"never blind-overwrite",
		"apiVersion: tclaude.dev/v1alpha1",
		"kind: ProcessTemplate",
		"explicit `type` on every node",
		"uniform `performer` blocks",
		"top-level `start`",
		"inline `next`",
		"For a new template, omit `layout`",
		"preserve the entire existing `layout`",
		"Saving still executes nothing",
		"stable actor identity",
	} {
		assert.Contains(t, body, required)
	}

	start := strings.Index(body, "```yaml\n")
	require.NotEqual(t, -1, start, "skill carries a generation-from-scratch example")
	start += len("```yaml\n")
	end := strings.Index(body[start:], "\n```")
	require.NotEqual(t, -1, end)
	parsed, err := model.Parse([]byte(body[start : start+end]))
	require.NoError(t, err)
	assert.False(t, parsed.Diagnostics.HasErrors(), "skill example diagnostics: %v", parsed.Diagnostics)
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
