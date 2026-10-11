package skillbundle

import (
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Description reads a bounded scalar from SKILL.md frontmatter. Invalid or
// absent metadata is optional: it must not prevent a valid skill transfer.
func (b Bundle) Description() string {
	for _, file := range b.Files {
		if file.Path != "SKILL.md" {
			continue
		}
		raw := string(file.Data)
		if len(raw) > 64<<10 {
			raw = raw[:64<<10]
		}
		lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
		if len(lines) < 3 || lines[0] != "---" {
			return ""
		}
		end := 0
		for i := 1; i < len(lines); i++ {
			if lines[i] == "---" {
				end = i
				break
			}
		}
		if end == 0 {
			return ""
		}
		var doc yaml.Node
		if yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &doc) != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
			return ""
		}
		pairs := doc.Content[0].Content
		for i := 0; i+1 < len(pairs); i += 2 {
			value := pairs[i+1]
			if pairs[i].Value == "description" && value.Kind == yaml.ScalarNode && value.Tag == "!!str" {
				text := strings.TrimSpace(value.Value)
				if len(text) <= 2048 {
					return text
				}
				text = text[:2048]
				for !utf8.ValidString(text) {
					text = text[:len(text)-1]
				}
				return text
			}
		}
	}
	return ""
}
