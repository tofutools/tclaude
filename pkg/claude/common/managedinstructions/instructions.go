// Package managedinstructions resolves operator-managed skills and rules,
// without discovering or modifying user-global harness instruction files.
package managedinstructions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/skillbundle"
)

// Import reads only an explicitly supplied item. Skill directory traversal uses
// the same no-follow, regular-file and credential checks as Boards.
func Import(kind, name, path string) ([]byte, error) {
	switch kind {
	case "skill":
		b, err := skillbundle.Export(path, name)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		if len(raw) > db.ManagedInstructionItemLimit {
			return nil, errors.New("skill exceeds 1 MiB")
		}
		return raw, nil
	case "rule":
		raw, _, err := skillbundle.ReadFile(filepath.Dir(path), filepath.Base(path), db.ManagedInstructionItemLimit)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(raw) || len(raw) == 0 {
			return nil, errors.New("rule must be nonempty UTF-8 text")
		}
		if len(configbundle.CredentialKinds(string(raw))) > 0 {
			return nil, errors.New("rule contains suspected credentials")
		}
		return raw, nil
	default:
		return nil, errors.New("expected skill or rule")
	}
}

// Validate verifies the content again at the daemon boundary.
func Validate(kind, name string, raw []byte) error {
	if !skillbundle.ValidName(name) || len(raw) == 0 || len(raw) > db.ManagedInstructionItemLimit {
		return errors.New("invalid name or content exceeds 1 MiB")
	}
	if kind == "skill" {
		b, err := skillbundle.Decode(raw)
		if err != nil {
			return err
		}
		if b.Name != name {
			return errors.New("skill name does not match bundle")
		}
		return nil
	}
	if kind != "rule" || !utf8.Valid(raw) || len(configbundle.CredentialKinds(string(raw))) > 0 {
		return errors.New("rule must be UTF-8 text without credentials")
	}
	return nil
}

// Resolve applies group-over-user precedence. Same-name skills from different
// groups must agree in digest, otherwise launch refuses with named conflicts.
// Rules remain additive and content-deduplicated, ordered user then groups.
func Resolve(groups []*db.AgentGroup) ([]db.ManagedInstruction, error) {
	groups = append([]*db.AgentGroup(nil), groups...)
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	ids := []int64{0}
	names := map[int64]string{0: "user"}
	for _, g := range groups {
		ids = append(ids, g.ID)
		names[g.ID] = g.Name
	}
	skills := map[string]db.ManagedInstruction{}
	groupSkills := map[string]db.ManagedInstruction{}
	rules := []db.ManagedInstruction{}
	seenRules := map[string]bool{}
	total := 0
	for _, id := range ids {
		for _, kind := range []string{"rule", "skill"} {
			rows, err := db.ListManagedInstructions(id, kind)
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				if kind == "rule" {
					if !seenRules[r.Digest] {
						seenRules[r.Digest] = true
						rules = append(rules, r)
					}
					continue
				}
				if id != 0 {
					if old, ok := groupSkills[r.Name]; ok && old.Digest != r.Digest {
						return nil, fmt.Errorf("managed skill %q conflicts between groups %q and %q", r.Name, names[old.GroupID], names[id])
					}
					groupSkills[r.Name] = r
				}
				skills[r.Name] = r
			}
		}
	}
	out := append([]db.ManagedInstruction(nil), rules...)
	keys := []string{}
	for name := range skills {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		out = append(out, skills[name])
	}
	for _, r := range out {
		total += len(r.Content)
	}
	if total > db.ManagedInstructionAgentLimit {
		return nil, errors.New("effective managed instructions exceed 16 MiB")
	}
	return out, nil
}

// Materialize creates only a private disposable launch directory. It never
// writes harness HOME, the project, or daemon-private storage. The launcher
// must grant read-only access to Root alone before referencing these paths.
type Launch struct {
	Root      string
	Body      string
	Resources []db.ManagedInstruction
}

func Materialize(resources []db.ManagedInstruction) (_ Launch, err error) {
	if len(resources) == 0 {
		return Launch{}, nil
	}
	root, err := os.MkdirTemp("", "tclaude-managed-")
	if err != nil {
		return Launch{}, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(root)
		}
	}()
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Launch{}, err
	}
	rules, skills := []string{}, []string{}
	for _, r := range resources {
		switch r.Kind {
		case "rule":
			rules = append(rules, fmt.Sprintf("Rule %q (%s):\n%s", r.Name, r.Scope, string(r.Content)))
		case "skill":
			b, e := skillbundle.Decode(r.Content)
			if e != nil {
				return Launch{}, e
			}
			for _, f := range b.Files {
				dest := filepath.Join(root, "skills", b.Name, filepath.FromSlash(f.Path))
				if e = os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
					return Launch{}, e
				}
				mode := os.FileMode(0444)
				if f.Executable {
					mode = 0555
				}
				if e = os.WriteFile(dest, f.Data, mode); e != nil {
					return Launch{}, e
				}
			}
			skills = append(skills, fmt.Sprintf("%s: %s", b.Name, filepath.Join(root, "skills", b.Name, "SKILL.md")))
		default:
			return Launch{}, errors.New("unknown managed instruction kind")
		}
	}
	if len(skills) > 0 {
		if err = os.MkdirAll(filepath.Join(root, ".claude-plugin"), 0700); err != nil {
			return Launch{}, err
		}
		if err = os.WriteFile(filepath.Join(root, ".claude-plugin", "plugin.json"), []byte(`{"name":"tclaude-managed","version":"0.1.0","description":"This launch only: operator-managed skills"}`), 0444); err != nil {
			return Launch{}, err
		}
	}
	if len(rules) > 0 {
		if err = os.WriteFile(filepath.Join(root, "RULES.md"), []byte(strings.Join(rules, "\n\n")), 0444); err != nil {
			return Launch{}, err
		}
	}
	sections := []string{}
	if len(rules) > 0 {
		sections = append(sections, "Managed rules — operator guidance, applies throughout this launch:\n\n"+strings.Join(rules, "\n\n"))
	}
	if len(skills) > 0 {
		sections = append(sections, "Managed skills — read the relevant SKILL.md on demand; these paths belong only to this launch:\n"+strings.Join(skills, "\n"))
	}
	body := strings.Join(sections, "\n\n")
	if err = os.WriteFile(filepath.Join(root, "INSTRUCTIONS.md"), []byte(body), 0444); err != nil {
		return Launch{}, err
	}
	return Launch{root, body, resources}, nil
}
