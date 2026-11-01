package pwf

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// A path a workflow carries either reveals who wrote it or points at something
// the importing node does not have. Neither makes a file invalid: the scan
// only says so, at export (the author can fix it before sharing) and at review
// (the importer knows what to expect). It is recomputed from the templates, so
// it is not part of workflow.json and can improve without a format change.

const (
	// PathHome is a path inside someone's home directory: it reveals a
	// username and will not exist on another node.
	PathHome = "home"
	// PathMachine is a path that will likely not exist on another node.
	PathMachine = "machine"

	// maxPathFindings bounds what a file can make a review list.
	maxPathFindings = 200
	maxPathShown    = 160
)

// PathFinding is one machine-specific path a workflow carries. Where is
// "<template-id>: <field>", the field spelled like a validation problem's path.
type PathFinding struct {
	Where string `json:"where"`
	Path  string `json:"path"`
	Kind  string `json:"kind"`
}

const pathSegment = `[A-Za-z0-9._+@%~-]`

var (
	// A path starts at the text's start or after a separator, so the path of a
	// URL, a {{ param }}/relative path and sed's s/a/b/ are not matched. The
	// optional ~ prefix takes ~/x and ~name/x.
	unixPathRE = regexp.MustCompile("(?:^|[\\s\"'`=:,;(\\[<>|])((?:~" + pathSegment + `*)?/` + pathSegment + `+(?:/` + pathSegment + `*)*)`)
	winPathRE  = regexp.MustCompile(`\b[A-Za-z]:\\+[^\s"'<>|*?:]+`)
	// Anywhere in a path, so WSL's /mnt/c/Users/name counts too.
	unixHomeRE = regexp.MustCompile(`/(?:home|Users)/` + pathSegment + `+`)
	winHomeRE  = regexp.MustCompile(`(?i)^[a-z]:\\+Users\\+[^\\]+`)
	tildeRE    = regexp.MustCompile(`^~[^/]+/`)
)

var (
	// Commands and argument values may name any path, so everything outside
	// these standard roots counts as machine-specific.
	standardRoots = map[string]bool{"bin": true, "sbin": true, "usr": true, "lib": true, "lib64": true, "dev": true, "proc": true, "sys": true, "tmp": true}
	// Prose is full of things shaped like paths (API routes, either/or lists),
	// so there only the roots that are machine-specific in practice count.
	machineRoots = map[string]bool{"home": true, "Users": true, "root": true, "opt": true, "srv": true, "mnt": true, "media": true, "Volumes": true, "private": true, "workspace": true}
)

// pathKind classifies a path found in a string, or says it is fine. value
// marks a string used as a value (a command, an argument, a default), as
// opposed to prose.
func pathKind(path string, value bool) string {
	if unixHomeRE.MatchString(path) || tildeRE.MatchString(path) {
		return PathHome
	}
	if strings.HasPrefix(path, "~/") {
		if value {
			return PathMachine
		}
		return ""
	}
	root, rest, _ := strings.Cut(path[1:], "/")
	if root == "var" && (rest == "tmp" || strings.HasPrefix(rest, "tmp/")) {
		return ""
	}
	if value {
		if standardRoots[root] {
			return ""
		}
		return PathMachine
	}
	if machineRoots[root] && rest != "" {
		return PathMachine
	}
	return ""
}

// findPaths lists the machine-specific paths in one string.
func findPaths(text string, value bool) []PathFinding {
	var found []PathFinding
	// A file: URL names a local path; other URLs do not.
	for _, m := range unixPathRE.FindAllStringSubmatch(strings.ReplaceAll(text, "file://", " "), -1) {
		if kind := pathKind(m[1], value); kind != "" {
			found = append(found, PathFinding{Path: m[1], Kind: kind})
		}
	}
	for _, p := range winPathRE.FindAllString(text, -1) {
		if winHomeRE.MatchString(p) {
			found = append(found, PathFinding{Path: p, Kind: PathHome})
		} else if value {
			found = append(found, PathFinding{Path: p, Kind: PathMachine})
		}
	}
	return found
}

// pathScan collects findings without repeats, up to maxPathFindings.
type pathScan struct {
	found []PathFinding
	seen  map[PathFinding]bool
}

func (s *pathScan) add(where string, findings []PathFinding) {
	for _, f := range findings {
		f.Where = where
		if len(f.Path) > maxPathShown {
			f.Path = strings.ToValidUTF8(f.Path[:maxPathShown], "") + "…"
		}
		if !s.seen[f] && len(s.found) < maxPathFindings {
			s.seen[f] = true
			s.found = append(s.found, f)
		}
	}
}

// addNew adds the findings whose path nothing has shown yet.
func (s *pathScan) addNew(where string, findings []PathFinding) {
	for _, f := range findings {
		if !slices.ContainsFunc(s.found, func(old PathFinding) bool { return old.Path == f.Path }) {
			s.add(where, []PathFinding{f})
		}
	}
}

// valueField says whether a field holds a value rather than prose.
func valueField(field string) bool {
	last := field[strings.LastIndex(field, ".")+1:]
	return last == "run" || last == "default" || strings.HasPrefix(last, "args[")
}

// walkStrings visits every string in a decoded JSON value, in a stable order.
func walkStrings(v any, field string, visit func(field, value string)) {
	switch t := v.(type) {
	case string:
		visit(field, t)
	case []any:
		for i, item := range t {
			walkStrings(item, fmt.Sprintf("%s[%d]", field, i), visit)
		}
	case map[string]any:
		for _, k := range sortedKeys(t) {
			walkStrings(t[k], strings.TrimPrefix(field+"."+k, "."), visit)
		}
	}
}

// scanTemplate scans every string of a template, so a field added later is
// covered, then its source for home paths the fields do not show: a comment
// or a map key travels in the file too.
func (s *pathScan) scanTemplate(t *Template) error {
	raw, err := json.Marshal(t.Parsed)
	if err != nil {
		return err
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	shown := map[string]bool{}
	walkStrings(doc, "", func(field, value string) {
		found := findPaths(value, valueField(field))
		s.add(t.Parsed.ID+": "+field, found)
		for _, f := range found {
			shown[f.Path] = true
		}
	})
	var rest []PathFinding
	for _, f := range findPaths(string(t.Source), false) {
		if f.Kind == PathHome && !shown[f.Path] {
			rest = append(rest, f)
		}
	}
	s.add(t.Parsed.ID+": source", rest)
	return nil
}

func scanPaths(title, summary string, readme []byte, templates []*Template) ([]PathFinding, error) {
	s := &pathScan{seen: map[PathFinding]bool{}}
	for _, t := range templates {
		if err := s.scanTemplate(t); err != nil {
			return nil, fmt.Errorf("pwf: scan %s: %w", t.Ref, err)
		}
	}
	// The title and summary usually repeat the root template's name and
	// description, so only what the templates did not show counts.
	s.addNew("title", findPaths(title, false))
	s.addNew("summary", findPaths(summary, false))
	s.addNew(ReadmePath, findPaths(string(readme), false))
	return s.found, nil
}

// ScanPaths lists the machine-specific paths a bundle would carry, without
// writing it: the same list a reader of the written file gets as Archive.Paths.
func ScanPaths(b Bundle) ([]PathFinding, error) {
	byRef, order, _, err := prepare(b)
	if err != nil {
		return nil, err
	}
	title, summary := workflowText(b, byRef[b.Root].Parsed)
	return scanPaths(title, summary, b.Readme, inOrder(byRef, order))
}

func inOrder(byRef map[string]*Template, order []string) []*Template {
	templates := make([]*Template, 0, len(order))
	for _, ref := range order {
		templates = append(templates, byRef[ref])
	}
	return templates
}
