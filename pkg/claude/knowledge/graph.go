package knowledge

import (
	"path"
	"regexp"
	"strings"
	"unicode"
)

// Link is one edge of the docs graph: a page, and a section of it if the link
// names one.
type Link struct {
	Path    string `json:"path"`
	Section string `json:"section,omitempty"`
	Text    string `json:"text,omitempty"`
}

var (
	markdownLink = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	headingLine  = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)
)

// isFence reports whether a line opens or closes a code block.
func isFence(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

// pageLinks lists the links in text, from the page at from, to other pages in
// pages. Code blocks and links out of the docs are not edges.
func pageLinks(from, text string, pages map[string]bool) []Link {
	var links []Link
	seen := map[string]bool{}
	inFence := false
	for line := range strings.SplitSeq(text, "\n") {
		if isFence(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		for _, match := range markdownLink.FindAllStringSubmatch(line, -1) {
			target, section, _ := strings.Cut(match[2], "#")
			if target == "" || strings.Contains(target, ":") {
				continue
			}
			resolved := path.Join(path.Dir(from), target)
			key := resolved + "#" + section
			if resolved == from || !pages[resolved] || seen[key] {
				continue
			}
			seen[key] = true
			links = append(links, Link{Path: resolved, Section: section, Text: match[1]})
		}
	}
	return links
}

// slugify turns a heading into its anchor, the way the docs site does, so a
// link's #fragment names the section it points at.
func slugify(heading string) string {
	var out strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(markdownLink.ReplaceAllString(heading, "$1"))) {
		switch {
		case r == '-' || unicode.IsSpace(r):
			dash = out.Len() > 0
		case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			if dash {
				out.WriteByte('-')
				dash = false
			}
			out.WriteRune(r)
		}
	}
	return out.String()
}

// sectionLines finds the heading whose anchor is section and returns its lines
// as a half-open range, up to the next heading at its level or above, along
// with every anchor on the page for a useful miss.
func sectionLines(lines []string, section string) (start, end int, found bool, anchors []string) {
	level := 0
	inFence := false
	for i, line := range lines {
		if isFence(line) {
			inFence = !inFence
			continue
		}
		match := headingLine.FindStringSubmatch(line)
		if inFence || match == nil {
			continue
		}
		anchor := slugify(match[2])
		anchors = append(anchors, anchor)
		if found {
			if end == 0 && len(match[1]) <= level {
				end = i
			}
			continue
		}
		if anchor == section {
			start, level, found = i, len(match[1]), true
		}
	}
	if found && end == 0 {
		end = len(lines)
	}
	return start, end, found, anchors
}
