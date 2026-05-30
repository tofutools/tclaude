package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// UserDir is where user workflow templates live: ~/.tclaude/workflows.
// Returns "" if the home directory cannot be determined.
func UserDir() string {
	cd := config.ConfigDir()
	if cd == "" {
		return ""
	}
	return filepath.Join(cd, "workflows")
}

// ProjectDir is where a repo's workflow templates live: <root>/.tclaude/workflows.
func ProjectDir(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, ".tclaude", "workflows")
}

// ListEntry summarises one discoverable template for listing. When the template
// failed to load/validate, Err is set and the structural fields are best-effort.
type ListEntry struct {
	Ref         string   `json:"ref"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Source      Source   `json:"source"`
	Dir         string   `json:"dir,omitempty"`
	NodeCount   int      `json:"node_count"`
	Err         string   `json:"err,omitempty"`
	Warnings    []string `json:"warnings,omitempty"` // non-fatal topology smells from load (sorted, deterministic)
}

// candidate is a not-yet-loaded template directory found during discovery.
type candidate struct {
	name   string
	dir    string // "" for embedded
	source Source
}

// Resolve loads a template by reference using default options. See ResolveOpts.
func Resolve(ref string, projectDirs ...string) (*Template, error) {
	return ResolveOpts(ref, ResolveOptions{}, projectDirs...)
}

// ResolveOpts loads a template by reference. A ref may be:
//
//   - qualified local: "project:name", "user:name", "example:name"
//   - external:        "dir:<path>" or "git:<url>[@<ref>][#<path>]"
//   - bare "name", searched project dirs → user dir → embedded examples.
//
// projectDirs are repo-local template directories (see ProjectDir). dir: and
// git: are external (third-party) sources whose specs carry path characters
// ('/', ':', '@', '#'), so they skip the single-segment name validation; see
// fetch.go for the git fetch/cache and the trust model. opts only affects git:.
func ResolveOpts(ref string, opts ResolveOptions, projectDirs ...string) (*Template, error) {
	source, spec, qualified := splitRef(ref)

	// External, path-bearing sources first — their spec is a path/url, not a
	// single-segment name, so validRefName must not run on it.
	if qualified {
		switch source {
		case SourceDir:
			return resolveDir(spec, ref)
		case SourceGit:
			return resolveGit(ref, opts)
		}
	}

	// Local sources use a single-segment name.
	name := spec
	if err := validRefName(name); err != nil {
		return nil, fmt.Errorf("cannot resolve %q: %w", ref, err)
	}
	if qualified {
		switch source {
		case SourceExample:
			return loadExample(name)
		case SourceUser:
			ud := UserDir()
			if ud == "" {
				return nil, fmt.Errorf("cannot resolve %q: no user config dir", ref)
			}
			return LoadDir(filepath.Join(ud, name), ref, SourceUser)
		case SourceProject:
			for _, pd := range projectDirs {
				if dir := filepath.Join(pd, name); isTemplateDir(dir) {
					return LoadDir(dir, ref, SourceProject)
				}
			}
			return nil, fmt.Errorf("project workflow %q not found", name)
		default:
			return nil, fmt.Errorf("unknown workflow source %q in ref %q", source, ref)
		}
	}

	// Bare name: search in priority order.
	for _, pd := range projectDirs {
		if dir := filepath.Join(pd, name); isTemplateDir(dir) {
			return LoadDir(dir, string(SourceProject)+":"+name, SourceProject)
		}
	}
	if ud := UserDir(); ud != "" {
		if dir := filepath.Join(ud, name); isTemplateDir(dir) {
			return LoadDir(dir, string(SourceUser)+":"+name, SourceUser)
		}
	}
	if slices.Contains(exampleNames(), name) {
		return loadExample(name)
	}
	return nil, fmt.Errorf("workflow %q not found in project, user, or example sources", name)
}

// List returns every discoverable template, deduplicated by name with project
// shadowing user shadowing example. Templates that fail to load are still
// returned, with Err set, so the dashboard can surface the problem.
func List(projectDirs ...string) []ListEntry {
	var cands []candidate
	for _, pd := range projectDirs {
		cands = append(cands, candidatesIn(pd, SourceProject)...)
	}
	cands = append(cands, candidatesIn(UserDir(), SourceUser)...)
	for _, n := range exampleNames() {
		cands = append(cands, candidate{name: n, source: SourceExample})
	}

	seen := map[string]bool{}
	var out []ListEntry
	for _, c := range cands {
		if seen[c.name] {
			continue // shadowed by a higher-priority source
		}
		seen[c.name] = true
		out = append(out, loadEntry(c))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func loadEntry(c candidate) ListEntry {
	ref := string(c.source) + ":" + c.name
	var (
		t   *Template
		err error
	)
	if c.source == SourceExample {
		t, err = loadExample(c.name)
	} else {
		t, err = LoadDir(c.dir, ref, c.source)
	}
	if err != nil {
		return ListEntry{Ref: ref, Name: c.name, Source: c.source, Dir: c.dir, Err: err.Error()}
	}
	return ListEntry{
		Ref:         ref,
		Name:        t.Name,
		Description: t.Description,
		Source:      c.source,
		Dir:         c.dir,
		NodeCount:   len(t.Nodes),
		Warnings:    t.Warnings, // populated by load's analyzeGraph
	}
}

// candidatesIn lists template subdirectories of dir.
func candidatesIn(dir string, source Source) []candidate {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []candidate
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		if isTemplateDir(sub) {
			out = append(out, candidate{name: e.Name(), dir: sub, source: source})
		}
	}
	return out
}

// isTemplateDir reports whether dir looks like a template (has a workflow.yaml).
func isTemplateDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "workflow.yaml"))
	return err == nil && !info.IsDir()
}

// validRefName rejects names that would escape the workflows directory or
// otherwise traverse the filesystem. A workflow name is a single path segment.
func validRefName(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("invalid workflow name %q", name)
	}
	return nil
}

// splitRef splits "source:name" into its parts. qualified is false for a bare
// name (no recognised source prefix).
func splitRef(ref string) (source Source, name string, qualified bool) {
	prefix, rest, found := strings.Cut(ref, ":")
	if !found {
		return "", ref, false
	}
	switch Source(prefix) {
	case SourceProject, SourceUser, SourceExample, SourceDir, SourceGit:
		// For dir:/git: the "name" is the rest of the ref verbatim (a path/url
		// spec), not a single-segment template name.
		return Source(prefix), rest, true
	default:
		// Not a recognised source — treat the whole thing as a bare name.
		return "", ref, false
	}
}
