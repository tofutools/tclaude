package workflow

import (
	"fmt"
	"regexp"
	"strings"
)

// parseMermaid parses the supported subset of mermaid flowchart syntax into a
// direction, the declared nodes, and the directed edges.
//
// Supported subset (anything outside this is either ignored or rejected with a
// clear error — see below):
//
//   - Header: a first meaningful line `flowchart <dir>` or `graph <dir>`, where
//     <dir> is one of TD, TB, BT, LR, RL (optional; defaults to TD).
//   - Node ids: [A-Za-z0-9_]+ .
//   - Node shapes (text is cosmetic, used only for labels): A, A[rect],
//     A(round), A([stadium]), A[[subroutine]], A[(cylinder)], A((circle)),
//     A{diamond}, A{{hexagon}}, A>flag].
//   - Edges, left-to-right only: -->  ---  -.->  -.-  ==>  ===  --x  --o ,
//     including mermaid's link "lengthening" with extra dashes/equals
//     (--->, ---->, ====>, etc.).
//   - Edge labels via the pipe form only: A -->|label| B .
//   - Chains: A --> B --> C . Multi-target with &: A --> B & C , A & B --> C .
//   - Statements separated by newlines or ';'.
//   - Ignored lines: '%%' comments, and subgraph/end/classDef/class/style/
//     linkStyle/click directives.
//
// Explicitly NOT supported (rejected with an error): reversed/bidirectional
// arrows (<--, <-->) and inline-text links (A -- text --> B). Use the pipe form
// for labels and write edges left-to-right.
func parseMermaid(src string) (direction string, nodes map[string]MermaidNode, edges []Edge, err error) {
	nodes = map[string]MermaidNode{}
	direction = "TD"

	lines := strings.Split(src, "\n")
	sawHeader := false
	subgraphDepth := 0
	for lineNo, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "%%") {
			continue
		}
		if !sawHeader {
			dir, ok := parseHeader(line)
			if !ok {
				return "", nil, nil, fmt.Errorf("line %d: expected a 'flowchart' or 'graph' header, got %q", lineNo+1, line)
			}
			if dir != "" {
				direction = dir
			}
			sawHeader = true
			continue
		}
		// Ignore the mermaid directives we don't model. Matched as exact first
		// tokens (not prefixes) so node ids like "endNode" or "classDefault" are
		// not mistaken for keywords. "end" is special: it closes a subgraph only
		// when one is open; at depth 0 it is an ordinary node id.
		switch firstToken(line) {
		case "subgraph":
			subgraphDepth++
			continue
		case "end":
			if subgraphDepth > 0 {
				subgraphDepth--
				continue
			}
		case "classDef", "class", "style", "linkStyle", "click", "direction":
			continue
		}
		for stmt := range strings.SplitSeq(line, ";") {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if err := parseStatement(stmt, nodes, &edges); err != nil {
				return "", nil, nil, fmt.Errorf("line %d: %w", lineNo+1, err)
			}
		}
	}
	if !sawHeader {
		return "", nil, nil, fmt.Errorf("empty chart: no 'flowchart'/'graph' header found")
	}
	return direction, nodes, edges, nil
}

// firstToken returns the leading whitespace-delimited token of a line.
func firstToken(line string) string {
	for i := 0; i < len(line); i++ {
		if line[i] == ' ' || line[i] == '\t' {
			return line[:i]
		}
	}
	return line
}

// parseHeader matches "flowchart"/"graph" optionally followed by a direction.
func parseHeader(line string) (dir string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", false
	}
	if fields[0] != "flowchart" && fields[0] != "graph" {
		return "", false
	}
	if len(fields) >= 2 {
		switch strings.ToUpper(fields[1]) {
		case "TD", "TB", "BT", "LR", "RL":
			return strings.ToUpper(fields[1]), true
		}
	}
	return "", true
}

// parseStatement parses one statement (a chain of node groups joined by links)
// recording node declarations into nodes and edges into edges.
func parseStatement(s string, nodes map[string]MermaidNode, edges *[]Edge) error {
	i := 0
	left, ni, err := parseNodeGroup(s, i, nodes)
	if err != nil {
		return err
	}
	i = ni
	if len(left) == 0 {
		return fmt.Errorf("expected a node, got %q", s)
	}
	for {
		i = skipSpace(s, i)
		if i >= len(s) {
			break
		}
		label, li, err := parseLink(s, i)
		if err != nil {
			return err
		}
		i = li
		right, ri, err := parseNodeGroup(s, i, nodes)
		if err != nil {
			return err
		}
		i = ri
		if len(right) == 0 {
			return fmt.Errorf("edge has no target node in %q", s)
		}
		for _, l := range left {
			for _, r := range right {
				*edges = append(*edges, Edge{From: l, To: r, Label: label})
			}
		}
		left = right
	}
	return nil
}

// parseNodeGroup parses one or more node tokens separated by '&'.
func parseNodeGroup(s string, i int, nodes map[string]MermaidNode) ([]string, int, error) {
	var group []string
	for {
		id, ni, err := parseNodeToken(s, i, nodes)
		if err != nil {
			return nil, i, err
		}
		i = ni
		group = append(group, id)
		i = skipSpace(s, i)
		if i < len(s) && s[i] == '&' {
			i++
			continue
		}
		return group, i, nil
	}
}

// parseNodeToken parses a node id and optional shape, recording the declaration.
func parseNodeToken(s string, i int, nodes map[string]MermaidNode) (string, int, error) {
	i = skipSpace(s, i)
	start := i
	for i < len(s) && isIDChar(s[i]) {
		i++
	}
	id := s[start:i]
	if id == "" {
		return "", i, fmt.Errorf("expected a node id at %q", s[start:])
	}
	shape, text, ni, matched := parseShape(s, i)
	if matched {
		i = ni
	}
	// First declaration with text wins; bare references never clobber a label.
	existing, ok := nodes[id]
	if !ok {
		nodes[id] = MermaidNode{ID: id, Text: text, Shape: shape}
	} else if existing.Text == "" && text != "" {
		existing.Text = text
		existing.Shape = shape
		nodes[id] = existing
	}
	return id, i, nil
}

// shapeDelims lists shape bracket pairs, longest opening first so doubled forms
// match before their single-char counterparts.
var shapeDelims = []struct {
	open, close, name string
}{
	{"((", "))", "circle"},
	{"([", "])", "stadium"},
	{"[[", "]]", "subroutine"},
	{"[(", ")]", "cylinder"},
	{"{{", "}}", "hexagon"},
	{"[", "]", "rect"},
	{"(", ")", "round"},
	{"{", "}", "diamond"},
	{">", "]", "flag"},
}

// parseShape parses an optional shape wrapper at i, returning the shape name,
// inner text, the index past the wrapper, and whether one matched.
func parseShape(s string, i int) (shape, text string, next int, matched bool) {
	for _, d := range shapeDelims {
		if !strings.HasPrefix(s[i:], d.open) {
			continue
		}
		rest := s[i+len(d.open):]
		idx := strings.Index(rest, d.close)
		if idx < 0 {
			continue // not actually this shape (unterminated); let a shorter delim try
		}
		inner := strings.TrimSpace(rest[:idx])
		inner = strings.Trim(inner, `"`)
		return d.name, inner, i + len(d.open) + idx + len(d.close), true
	}
	return "", "", i, false
}

// linkOpRe matches a supported left-to-right edge operator at the start of the
// string. It accepts mermaid's link "lengthening" with extra dashes/equals:
// solid -->, --->, ----> ; thick ==>, ===> ; dotted -.->, -..-> ; open ---,
// ----, === ; and --x / --o heads. Reversed/bidirectional arrows are rejected
// separately in parseLink.
var linkOpRe = regexp.MustCompile(`^(?:-\.+->|-\.+-|={2,}>|={3,}|-{2,}>|-{2,}[xo]|-{3,})`)

// parseLink parses an edge operator and optional pipe label at i.
func parseLink(s string, i int) (label string, next int, err error) {
	i = skipSpace(s, i)
	if i < len(s) && s[i] == '<' {
		return "", i, fmt.Errorf("reversed/bidirectional arrows (<-- / <-->) are not supported; write edges left-to-right at %q", s[i:])
	}
	loc := linkOpRe.FindStringIndex(s[i:])
	if loc == nil {
		return "", i, fmt.Errorf("expected an edge operator (e.g. -->) at %q", s[i:])
	}
	i += loc[1]
	j := skipSpace(s, i)
	if j < len(s) && s[j] == '|' {
		end := strings.IndexByte(s[j+1:], '|')
		if end < 0 {
			return "", i, fmt.Errorf("unterminated edge label (missing closing '|') at %q", s[j:])
		}
		label = strings.TrimSpace(s[j+1 : j+1+end])
		i = j + 1 + end + 1
	}
	return label, i, nil
}

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

func isIDChar(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}
