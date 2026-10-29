package agentd

import (
	"strings"
	"testing"
)

func TestCompanionPeekLinesKeepsTheLastNonBlankRows(t *testing.T) {
	var rows []string
	for i := range 30 {
		rows = append(rows, "row "+strings.Repeat("x", i%3))
	}
	screen := strings.Join(rows, "\n") + "\n\n   \n\n"
	got := companionPeekLinesOf(screen)
	if len(got) != companionPeekLines {
		t.Fatalf("got %d lines, want %d", len(got), companionPeekLines)
	}
	if got[len(got)-1] != rows[len(rows)-1] {
		t.Fatalf("last line = %q, want %q (trailing blank rows must be dropped)", got[len(got)-1], rows[len(rows)-1])
	}
}

func TestCompanionPeekLinesClipsOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("é", companionPeekLineMax) // 2 bytes each
	got := companionPeekLinesOf(long)
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1", len(got))
	}
	if !strings.HasSuffix(got[0], "…") || len(got[0]) > companionPeekLineMax+len("…") {
		t.Fatalf("line not clipped: %d bytes", len(got[0]))
	}
	if !strings.HasPrefix(got[0], "é") || strings.ContainsRune(strings.TrimSuffix(got[0], "…"), '�') {
		t.Fatalf("clip split a rune: %q", got[0][len(got[0])-8:])
	}
}

func TestCompanionPeekLinesOfAnEmptyScreen(t *testing.T) {
	if got := companionPeekLinesOf("\n\n  \n"); len(got) != 0 {
		t.Fatalf("got %q, want no lines", got)
	}
}
