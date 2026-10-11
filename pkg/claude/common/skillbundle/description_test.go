package skillbundle

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestSkillDescriptionFrontmatterIsOptionalAndBounded(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"---\ndescription: Useful skill\n---\nbody", "Useful skill"},
		{"---\r\ndescription: >-\r\n  Record a\r\n  demo.\r\n---\r\nbody", "Record a demo."},
		{"# no frontmatter", ""}, {"---\ndescription: [broken\n---", ""},
		{"---\ndescription: &a [many, aliases]\n---", ""},
	} {
		b := Bundle{Files: []File{{Path: "SKILL.md", Data: []byte(tc.raw)}}}
		require.Equal(t, tc.want, b.Description())
	}
	b := Bundle{Files: []File{{Path: "SKILL.md", Data: []byte("---\ndescription: " + strings.Repeat("é", 2000) + "\n---\n")}}}
	require.LessOrEqual(t, len(b.Description()), 2048)
	require.True(t, utf8.ValidString(b.Description()))
}
