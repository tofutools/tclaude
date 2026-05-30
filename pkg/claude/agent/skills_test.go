package agent

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestBundledSkillsMaterialize guards the bundledSkills ↔ //go:embed contract in
// the forward direction: every skill registered in bundledSkills must both be
// present in the embedded FS and materialize a non-empty SKILL.md through the
// real writeSkillTree path that InstallSkills uses. A skill added to
// bundledSkills but forgotten from the //go:embed directive makes
// writeSkillTree's fs.WalkDir over skills/<name> fail here — turning a
// silent-at-install bug into a build-time test failure.
//
// It writes into t.TempDir rather than calling InstallSkills directly so the
// test never touches the real ~/.claude/skills tree.
func TestBundledSkillsMaterialize(t *testing.T) {
	if len(bundledSkills) == 0 {
		t.Fatal("bundledSkills is empty — no skills would ship")
	}
	for _, name := range bundledSkills {
		t.Run(name, func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), name)
			if err := writeSkillTree(name, dst); err != nil {
				t.Fatalf("writeSkillTree(%q): %v — is skills/%s/SKILL.md listed in the //go:embed directive?", name, err, name)
			}
			data, err := os.ReadFile(filepath.Join(dst, "SKILL.md"))
			if err != nil {
				t.Fatalf("installed %s SKILL.md: %v", name, err)
			}
			if strings.TrimSpace(string(data)) == "" {
				t.Fatalf("installed %s SKILL.md is empty", name)
			}
		})
	}
}

// TestNoOrphanEmbeddedSkills guards the reverse direction: every skill directory
// embedded under skills/ must be registered in bundledSkills. InstallSkills only
// iterates bundledSkills, so a skill that is embedded (and on disk) but missing
// from the slice would be silently never installed; this catches that.
func TestNoOrphanEmbeddedSkills(t *testing.T) {
	entries, err := fs.ReadDir(skillsFS, "skills")
	if err != nil {
		t.Fatalf("read embedded skills dir: %v", err)
	}
	var embedded int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		embedded++
		if !slices.Contains(bundledSkills, e.Name()) {
			t.Errorf("embedded skill %q is not in bundledSkills — it ships in the binary but InstallSkills would never write it", e.Name())
		}
	}
	if embedded == 0 {
		t.Fatal("no skill directories found in the embedded FS")
	}
}
