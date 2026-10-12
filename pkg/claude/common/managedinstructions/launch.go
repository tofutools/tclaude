package managedinstructions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/skillbundle"
)

// ClaudeNativeAvailable is checked only for a managed launch with skills; it
// uses local help, never a model request. Old/absent CLIs keep the briefing path.
func ClaudeNativeAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "claude", "--help").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--plugin-dir ") {
			return true
		}
	}
	return false
}
func HasNativeSkills(root string) bool {
	_, err := os.Stat(filepath.Join(root, ".claude-plugin", "plugin.json"))
	return err == nil
}

// VerifyLaunchRoot refuses arbitrary or altered runtime read grants. The daemon
// registered these exact bytes in private state before forking the launcher.
func VerifyLaunchRoot(root string) error {
	if !filepath.IsAbs(root) || !strings.HasPrefix(filepath.Base(root), "tclaude-managed-") {
		return errors.New("invalid managed launch directory")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if canonical != root {
		return errors.New("managed launch directory became a symlink")
	}
	stored, err := db.GetManagedInstructionLaunchByRoot(root)
	if err != nil {
		return err
	}
	if stored == nil {
		return errors.New("managed launch directory is not registered")
	}
	var resources []db.ManagedInstruction
	if err = json.Unmarshal([]byte(stored.Resources), &resources); err != nil {
		return err
	}
	expected := map[string][]byte{}
	rules := []string{}
	total := 0
	for _, r := range resources {
		if err = Validate(r.Kind, r.Name, r.Content); err != nil {
			return err
		}
		total += len(r.Content)
		if total > db.ManagedInstructionAgentLimit {
			return errors.New("managed launch content exceeds 16 MiB")
		}
		if r.Kind == "rule" {
			rules = append(rules, fmt.Sprintf("Rule %q (%s):\n%s", r.Name, r.Scope, string(r.Content)))
		}
		if r.Kind == "skill" {
			b, e := skillbundle.Decode(r.Content)
			if e != nil {
				return e
			}
			for _, f := range b.Files {
				expected[filepath.ToSlash(filepath.Join("skills", b.Name, f.Path))] = f.Data
			}
		}
	}
	if len(expected) > 0 {
		expected[".claude-plugin/plugin.json"] = []byte(`{"name":"tclaude-managed","version":"0.1.0","description":"This launch only: operator-managed skills"}`)
	}
	if len(rules) > 0 {
		expected["RULES.md"] = []byte(strings.Join(rules, "\n\n"))
	}
	expected["INSTRUCTIONS.md"] = []byte(stored.Body)
	for path, want := range expected {
		got, _, e := skillbundle.ReadFile(root, path, int64(db.ManagedInstructionAgentLimit+(8<<20)))
		if e != nil {
			return e
		}
		if !bytes.Equal(got, want) {
			return errors.New("managed launch content changed")
		}
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("managed launch contains a symlink")
		}
		if entry.IsDir() {
			return nil
		}
		relative, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		if _, ok := expected[filepath.ToSlash(relative)]; !ok {
			return errors.New("managed launch contains unregistered files")
		}
		return nil
	})
}

func RemoveLaunchRoot(root string) error {
	if root == "" {
		return nil
	}
	if !filepath.IsAbs(root) || !strings.HasPrefix(filepath.Base(root), "tclaude-managed-") {
		return errors.New("refusing to remove a non-managed launch directory")
	}
	return os.RemoveAll(root)
}
