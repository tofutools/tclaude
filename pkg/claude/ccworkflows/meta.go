package ccworkflows

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// MetaPhase is one entry of a workflow's `meta.phases` array.
type MetaPhase struct {
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// ScriptMeta is the statically-extracted `meta` object of a workflow script.
// It carries only the declarative header — never the executable body.
type ScriptMeta struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Phases      []MetaPhase `json:"phases,omitempty"`
	Model       string      `json:"model,omitempty"`
	WhenToUse   string      `json:"whenToUse,omitempty"`
}

// SavedScript is a saved/named workflow template on disk plus its parsed meta.
type SavedScript struct {
	// Name is the file's basename without the .js extension. CC keys saved
	// workflows by filename; meta.name usually matches but is not guaranteed.
	Name string `json:"name"`
	// Path is the absolute path to the .js file.
	Path string `json:"path"`
	// Meta is the parsed header. Scope is "user" or "project".
	Meta  ScriptMeta `json:"meta"`
	Scope string     `json:"scope"`
}

// metaAssignRe locates the `meta = {` assignment, tolerating `export const`,
// `const`, `let`, `var`, or a bare assignment, with arbitrary whitespace.
var metaAssignRe = regexp.MustCompile(`(?:export\s+)?(?:const|let|var)?\s*\bmeta\b\s*=\s*\{`)

// ParseScriptMeta statically extracts the `meta` object from a workflow script
// source without executing any JavaScript. The `meta` literal is contractually
// a pure literal, so a tolerant static parse is well-defined.
func ParseScriptMeta(src string) (ScriptMeta, error) {
	loc := metaAssignRe.FindStringIndex(src)
	if loc == nil {
		return ScriptMeta{}, fmt.Errorf("no `meta = {` assignment found")
	}
	// Point the lexer at the opening brace (loc[1] is just past it).
	braceOff := loc[1] - 1
	v, _, err := parseJSValue(src, braceOff)
	if err != nil {
		return ScriptMeta{}, fmt.Errorf("parsing meta object: %w", err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return ScriptMeta{}, fmt.Errorf("meta is not an object literal (got %T)", v)
	}
	return metaFromMap(obj), nil
}

func metaFromMap(obj map[string]any) ScriptMeta {
	m := ScriptMeta{}
	if s, ok := asString(obj["name"]); ok {
		m.Name = s
	}
	if s, ok := asString(obj["description"]); ok {
		m.Description = s
	}
	if s, ok := asString(obj["model"]); ok {
		m.Model = s
	}
	if s, ok := asString(obj["whenToUse"]); ok {
		m.WhenToUse = s
	}
	if phases, ok := obj["phases"].([]any); ok {
		for _, p := range phases {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			mp := MetaPhase{}
			if s, ok := asString(pm["title"]); ok {
				mp.Title = s
			}
			if s, ok := asString(pm["detail"]); ok {
				mp.Detail = s
			}
			m.Phases = append(m.Phases, mp)
		}
	}
	return m
}

// SavedScriptsDirs returns the candidate directories that hold saved workflow
// templates, most-global first: the user dir (~/.claude/workflows/saved) and,
// if projectDir is non-empty, its project-local mirror
// (<projectDir>/.claude/workflows/saved). Missing dirs are still returned;
// callers (and ListSavedScripts) tolerate their absence.
func SavedScriptsDirs(projectDir string) []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".claude", "workflows", "saved"))
	}
	if projectDir != "" {
		dirs = append(dirs, filepath.Join(projectDir, ".claude", "workflows", "saved"))
	}
	return dirs
}

// ListSavedScripts enumerates saved workflow templates (*.js) under the given
// directories and statically parses each one's meta. A scope label is attached
// per directory: the first dir is "user", any later dir is "project". Missing
// directories are skipped silently; a script whose meta fails to parse is
// included with an empty Meta (its Name still set from the filename) so the
// caller can surface it rather than dropping it.
//
// Results are sorted by Name (then Path) for stable output.
func ListSavedScripts(dirs ...string) ([]SavedScript, error) {
	var out []SavedScript
	for idx, dir := range dirs {
		scope := "project"
		if idx == 0 {
			scope = "user"
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("reading saved-scripts dir %q: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			name := strings.TrimSuffix(e.Name(), ".js")
			ss := SavedScript{Name: name, Path: path, Scope: scope}
			if data, err := os.ReadFile(path); err == nil {
				if meta, err := ParseScriptMeta(string(data)); err == nil {
					ss.Meta = meta
				}
			}
			out = append(out, ss)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}
