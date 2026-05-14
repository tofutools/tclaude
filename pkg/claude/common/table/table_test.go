package table

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTruncateWithEllipsis(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"empty string", "", 10, ""},
		{"fits exactly", "hello", 5, "hello"},
		{"fits with room", "hi", 5, "hi"},
		{"needs truncation", "hello world", 5, "hell…"},
		{"maxLen 1", "hello", 1, "…"},
		{"maxLen 0", "hello", 0, ""},
		{"negative maxLen", "hello", -5, ""},
		{"unicode string", "héllo", 5, "héllo"},
		{"unicode truncation", "héllo wörld", 6, "héllo…"},
		{"emoji fits", "hello 👋", 8, "hello 👋"},
		{"emoji truncate", "hello 👋 world", 8, "hello …"},
		{"wide char padding", "⚡test", 6, "⚡test"},
		{"wide char truncate", "⚡test", 5, "⚡te…"},
		{"exactly truncation point", "hello", 6, "hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateWithEllipsis(tt.input, tt.maxLen)
			assert.Equal(t, tt.want, got, "TruncateWithEllipsis(%q, %d)", tt.input, tt.maxLen)
		})
	}
}

func TestShortenPath(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		maxLen int
		want   string
	}{
		{"empty path", "", 10, ""},
		{"fits exactly", "file.go", 7, "file.go"},
		{"simple file fits", "main.go", 20, "main.go"},
		{"path needs shortening", "/home/user/project/src/main.go", 15, "…/src/main.go"},
		{"just filename", "/very/long/path/to/file.go", 7, "file.go"},
		{"filename too long", "/path/to/very_long_filename.go", 10, "very_long…"},
		{"maxLen 0", "file.go", 0, ""},
		{"single component path", "filename.txt", 20, "filename.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShortenPath(tt.path, tt.maxLen)
			assert.Equal(t, tt.want, got, "ShortenPath(%q, %d)", tt.path, tt.maxLen)
		})
	}
}

func TestTruncateFromStart(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"empty string", "", 10, ""},
		{"fits exactly", "hello", 5, "hello"},
		{"fits with room", "hi", 5, "hi"},
		{"needs truncation", "hello world", 5, "…orld"},
		{"maxLen 1", "hello", 1, "…"},
		{"maxLen 0", "hello", 0, ""},
		{"path truncation", "/home/user/projects/myapp", 15, "…projects/myapp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateFromStart(tt.input, tt.maxLen)
			assert.Equal(t, tt.want, got, "TruncateFromStart(%q, %d)", tt.input, tt.maxLen)
		})
	}
}

func TestPadFunctions(t *testing.T) {
	t.Run("PadRight", func(t *testing.T) {
		tests := []struct {
			input string
			width int
			want  string
		}{
			{"hi", 5, "hi   "},
			{"hello", 5, "hello"},
			{"hello world", 5, "hell…"},
			{"", 3, "   "},
			// Wide character tests - ⚡ has display width 2
			{"⚡", 2, "⚡"},   // Exactly fits
			{"⚡", 3, "⚡ "},  // Needs 1 space padding
			{"⚡", 4, "⚡  "}, // Needs 2 spaces padding
			{" ▷", 2, " ▷"}, // Narrow chars fit
			// ANSI styled text - escape codes should not affect width
			{lipgloss.NewStyle().Foreground(lipgloss.Color("46")).Render("⚡"), 2,
				lipgloss.NewStyle().Foreground(lipgloss.Color("46")).Render("⚡")}, // Styled emoji fits in 2
		}
		for _, tt := range tests {
			got := PadRight(tt.input, tt.width)
			assert.Equal(t, tt.want, got, "PadRight(%q, %d)", tt.input, tt.width)
		}
	})

	t.Run("PadLeft", func(t *testing.T) {
		tests := []struct {
			input string
			width int
			want  string
		}{
			{"hi", 5, "   hi"},
			{"hello", 5, "hello"},
			{"hello world", 5, "hell…"},
		}
		for _, tt := range tests {
			got := PadLeft(tt.input, tt.width)
			assert.Equal(t, tt.want, got, "PadLeft(%q, %d)", tt.input, tt.width)
		}
	})

	t.Run("PadCenter", func(t *testing.T) {
		tests := []struct {
			input string
			width int
			want  string
		}{
			{"hi", 6, "  hi  "},
			{"hi", 5, " hi  "},
			{"hello", 5, "hello"},
		}
		for _, tt := range tests {
			got := PadCenter(tt.input, tt.width)
			assert.Equal(t, tt.want, got, "PadCenter(%q, %d)", tt.input, tt.width)
		}
	})
}

func TestCalculateColumnWidths(t *testing.T) {
	tests := []struct {
		name      string
		columns   []Column
		termWidth int
		padding   int
		want      []int
	}{
		{
			name:      "empty columns",
			columns:   []Column{},
			termWidth: 80,
			padding:   2,
			want:      nil,
		},
		{
			name: "all fixed width",
			columns: []Column{
				{Header: "A", Width: 10},
				{Header: "B", Width: 20},
				{Header: "C", Width: 15},
			},
			termWidth: 80,
			padding:   2,
			want:      []int{10, 20, 15},
		},
		{
			name: "one flexible column",
			columns: []Column{
				{Header: "A", Width: 10},
				{Header: "B"},
				{Header: "C", Width: 10},
			},
			termWidth: 80,
			padding:   2,
			// 80 - (10 + 10) - (2 * 2) = 56 for flexible
			want: []int{10, 56, 10},
		},
		{
			name: "flexible with min width",
			columns: []Column{
				{Header: "A", Width: 10},
				{Header: "B", MinWidth: 100},
			},
			termWidth: 80,
			padding:   2,
			// Should respect min width even if over budget
			want: []int{10, 100},
		},
		{
			name: "flexible with max width",
			columns: []Column{
				{Header: "A"},
				{Header: "B", MaxWidth: 20},
			},
			termWidth: 80,
			padding:   2,
			// 80 - 2 = 78 available, split by weight but B capped at 20
			// B gets min(39, 20) = 20, remainder 58 goes to A
			want: []int{58, 20},
		},
		{
			name: "weighted distribution",
			columns: []Column{
				{Header: "A", Weight: 1},
				{Header: "B", Weight: 2},
			},
			termWidth: 62,
			padding:   2,
			// 62 - 2 = 60 available
			// A gets 60 * (1/3) = 20, B gets 60 * (2/3) = 40
			want: []int{20, 40},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateColumnWidths(tt.columns, tt.termWidth, tt.padding)
			require.Len(t, got, len(tt.want), "CalculateColumnWidths() length = %d, want %d", len(got), len(tt.want))
			for i := range got {
				assert.Equal(t, tt.want[i], got[i], "CalculateColumnWidths()[%d]", i)
			}
		})
	}
}

func TestContentAwareWidths(t *testing.T) {
	// Create table with flexible column that has MaxWidth 50
	tbl := New(
		Column{Header: "ID", Width: 5},
		Column{Header: "PATH", MinWidth: 10, MaxWidth: 50, Truncate: true},
	)
	tbl.SetTerminalWidth(100)

	// Add rows with short content (max 15 chars)
	tbl.AddRow(Row{Cells: []string{"1", "/home/user"}})     // 10 chars
	tbl.AddRow(Row{Cells: []string{"2", "/home/user/foo"}}) // 14 chars

	widths := tbl.CalculateWidths()

	// PATH column should be capped at content width (14), not expand to MaxWidth (50)
	// or fill remaining space
	assert.LessOrEqual(t, widths[1], 20, "PATH column width = %d, expected <= 20 (content-aware)", widths[1])
	assert.GreaterOrEqual(t, widths[1], 14, "PATH column width = %d, expected >= 14 (content width)", widths[1])
}

func TestTableNew(t *testing.T) {
	tbl := New(
		Column{Header: "ID", Width: 10},
		Column{Header: "Name", MinWidth: 20},
	)

	assert.Len(t, tbl.Columns, 2, "expected 2 columns")
	assert.Equal(t, -1, tbl.SelectedIndex, "expected SelectedIndex -1")
	assert.Equal(t, 2, tbl.Padding, "expected Padding 2")
}

func TestTableAddRow(t *testing.T) {
	tbl := New(Column{Header: "A"}, Column{Header: "B"})

	tbl.AddRow(Row{Cells: []string{"1", "2"}})
	tbl.AddRow(Row{Cells: []string{"3", "4"}})

	assert.Len(t, tbl.Rows, 2, "expected 2 rows")

	tbl.ClearRows()
	assert.Len(t, tbl.Rows, 0, "expected 0 rows after clear")
}

func TestTableRenderHeader(t *testing.T) {
	tbl := New(
		Column{Header: "ID", Width: 5},
		Column{Header: "NAME", Width: 10},
	)
	tbl.SetTerminalWidth(80)

	header := tbl.RenderHeader()

	assert.Contains(t, header, "ID", "header should contain 'ID'")
	assert.Contains(t, header, "NAME", "header should contain 'NAME'")
}

func TestTableRenderHeaderWithSort(t *testing.T) {
	tbl := New(
		Column{Header: "ID", Width: 5},
		Column{Header: "NAME", Width: 10},
	)
	tbl.SetTerminalWidth(80)
	tbl.Sort = SortConfig{Column: 1, Direction: SortDesc}

	header := tbl.RenderHeader()

	assert.Contains(t, header, "▼", "header should contain sort indicator ▼")
}

func TestTableRenderSeparator(t *testing.T) {
	tbl := New(
		Column{Header: "A", Width: 5},
		Column{Header: "B", Width: 10},
	)
	tbl.SetTerminalWidth(80)

	sep := tbl.RenderSeparator()

	// Total width should be 5 + 2 (padding) + 10 = 17 display cells
	displayWidth := StringWidth(sep)
	assert.Equal(t, 17, displayWidth, "separator display width")
}

func TestTableRenderRows(t *testing.T) {
	tbl := New(
		Column{Header: "ID", Width: 5},
		Column{Header: "NAME", Width: 10},
	)
	tbl.SetTerminalWidth(80)
	tbl.AddRow(Row{Cells: []string{"1", "Alice"}})
	tbl.AddRow(Row{Cells: []string{"2", "Bob"}})

	rows := tbl.RenderRows()

	assert.Contains(t, rows, "1", "rows should contain '1'")
	assert.Contains(t, rows, "Alice", "rows should contain 'Alice'")
	assert.Contains(t, rows, "Bob", "rows should contain 'Bob'")
}

func TestTableViewport(t *testing.T) {
	tbl := New(Column{Header: "ID", Width: 5})
	tbl.SetTerminalWidth(80)

	// Add 10 rows
	for i := range 10 {
		tbl.AddRow(Row{Cells: []string{string(rune('A' + i))}})
	}

	tbl.ViewportHeight = 3
	tbl.ViewportOffset = 0

	visible := tbl.VisibleRows()
	assert.Len(t, visible, 3, "expected 3 visible rows")
	assert.Equal(t, "A", visible[0].Cells[0], "first visible row should be 'A'")

	tbl.ViewportOffset = 5
	visible = tbl.VisibleRows()
	assert.Equal(t, "F", visible[0].Cells[0], "first visible row should be 'F'")
}

func TestTableScrollIndicator(t *testing.T) {
	tbl := New(Column{Header: "ID", Width: 5})
	tbl.SetTerminalWidth(80)

	for range 10 {
		tbl.AddRow(Row{Cells: []string{"x"}})
	}

	// No viewport = no scroll indicator
	assert.False(t, tbl.NeedsScrollIndicator(), "should not need scroll indicator without viewport")

	tbl.ViewportHeight = 5
	assert.True(t, tbl.NeedsScrollIndicator(), "should need scroll indicator with viewport smaller than rows")

	tbl.ViewportOffset = 0
	assert.Equal(t, 0, tbl.ScrollPercentage(), "scroll at top should be 0%%")

	tbl.ViewportOffset = 5 // max offset for 10 rows with viewport 5
	assert.Equal(t, 100, tbl.ScrollPercentage(), "scroll at bottom should be 100%%")
}

func TestTableEnsureCursorVisible(t *testing.T) {
	tbl := New(Column{Header: "ID", Width: 5})
	tbl.SetTerminalWidth(80)

	for range 10 {
		tbl.AddRow(Row{Cells: []string{"x"}})
	}

	tbl.ViewportHeight = 3
	tbl.ViewportOffset = 0
	tbl.SelectedIndex = 5

	tbl.EnsureCursorVisible()

	// Selection at 5, viewport 3, offset should be 3 to show rows 3,4,5
	assert.Equal(t, 3, tbl.ViewportOffset, "viewport offset should be 3")

	// Move selection up
	tbl.SelectedIndex = 1
	tbl.EnsureCursorVisible()
	assert.Equal(t, 1, tbl.ViewportOffset, "viewport offset should be 1")
}

func TestTableMoveSelection(t *testing.T) {
	tbl := New(Column{Header: "ID", Width: 5})
	tbl.SetTerminalWidth(80)

	for range 5 {
		tbl.AddRow(Row{Cells: []string{"x"}})
	}

	tbl.SelectedIndex = 0
	tbl.ViewportHeight = 3

	tbl.MoveSelection(1)
	assert.Equal(t, 1, tbl.SelectedIndex, "selection should be 1")

	tbl.MoveSelection(10) // Should clamp to max
	assert.Equal(t, 4, tbl.SelectedIndex, "selection should be 4")

	tbl.MoveSelection(-10) // Should clamp to 0
	assert.Equal(t, 0, tbl.SelectedIndex, "selection should be 0")
}

func TestTableSelectedRow(t *testing.T) {
	tbl := New(Column{Header: "ID", Width: 5})
	tbl.AddRow(Row{Cells: []string{"A"}})
	tbl.AddRow(Row{Cells: []string{"B"}})

	assert.Nil(t, tbl.SelectedRow(), "should return nil when no selection")

	tbl.SelectedIndex = 1
	row := tbl.SelectedRow()
	require.NotNil(t, row, "should return row when selected")
	assert.Equal(t, "B", row.Cells[0], "selected row should be 'B'")
}

func TestTableRender(t *testing.T) {
	tbl := New(
		Column{Header: "ID", Width: 5},
		Column{Header: "NAME", Width: 10},
	)
	tbl.SetTerminalWidth(80)
	tbl.AddRow(Row{Cells: []string{"1", "Alice"}})

	output := tbl.Render()

	// Should contain header, separator, and row
	lines := strings.Split(output, "\n")
	assert.Len(t, lines, 3, "expected 3 lines")
}

func TestTableRowStyle(t *testing.T) {
	tbl := New(Column{Header: "ID", Width: 10})
	tbl.SetTerminalWidth(80)

	redStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("red"))
	tbl.AddRow(Row{Cells: []string{"normal"}})
	tbl.AddRow(Row{Cells: []string{"styled"}, Style: redStyle})

	rows := tbl.RenderRows()

	// Both rows should be present
	assert.Contains(t, rows, "normal", "should contain 'normal'")
	assert.Contains(t, rows, "styled", "should contain 'styled'")
}

func TestSortStateToggle(t *testing.T) {
	s := SortState{}

	// First toggle: none -> asc
	s.Toggle("id")
	assert.Equal(t, "id", s.Key, "after first toggle: Key, want id")
	assert.Equal(t, SortAsc, s.Direction, "after first toggle: Dir, want Asc")

	// Second toggle same key: asc -> desc
	s.Toggle("id")
	assert.Equal(t, "id", s.Key, "after second toggle: Key, want id")
	assert.Equal(t, SortDesc, s.Direction, "after second toggle: Dir, want Desc")

	// Third toggle same key: desc -> none
	s.Toggle("id")
	assert.Equal(t, "", s.Key, "after third toggle: Key, want empty")
	assert.Equal(t, SortNone, s.Direction, "after third toggle: Dir, want None")

	// Toggle different key while sorted
	s.Toggle("id")
	s.Toggle("name")
	assert.Equal(t, "name", s.Key, "after switching column: Key, want name")
	assert.Equal(t, SortAsc, s.Direction, "after switching column: Dir, want Asc")
}

func TestSortStateToConfig(t *testing.T) {
	columns := []Column{
		{Header: "", Width: 2},
		{Header: "ID", Width: 10, SortKey: "id"},
		{Header: "NAME", MinWidth: 20, SortKey: "name"},
		{Header: "DATE", Width: 16, SortKey: "date"},
	}

	// No sort
	s := SortState{}
	cfg := s.ToConfig(columns)
	assert.Equal(t, -1, cfg.Column, "empty sort: Column, want -1")
	assert.Equal(t, SortNone, cfg.Direction, "empty sort: Dir, want None")

	// Sort by name (column index 2)
	s = SortState{Key: "name", Direction: SortDesc}
	cfg = s.ToConfig(columns)
	assert.Equal(t, 2, cfg.Column, "sort by name: Column, want 2")
	assert.Equal(t, SortDesc, cfg.Direction, "sort by name: Dir, want Desc")

	// Sort by unknown key
	s = SortState{Key: "unknown", Direction: SortAsc}
	cfg = s.ToConfig(columns)
	assert.Equal(t, -1, cfg.Column, "sort by unknown: Column, want -1")
}

func TestHandleSortKey(t *testing.T) {
	columns := []Column{
		{Header: "", Width: 2},                          // Not sortable
		{Header: "ID", Width: 10, SortKey: "id"},        // Sortable #1
		{Header: "NAME", MinWidth: 20, SortKey: "name"}, // Sortable #2
		{Header: "TITLE", MinWidth: 30},                 // Not sortable
		{Header: "DATE", Width: 16, SortKey: "date"},    // Sortable #3
	}

	s := SortState{}

	// Press "1" -> sorts by ID (1st sortable column)
	assert.True(t, s.HandleSortKey(columns, "1"), "HandleSortKey('1') should return true")
	assert.Equal(t, "id", s.Key, "after pressing 1: Key")

	// Press "2" -> sorts by NAME (2nd sortable column)
	s = SortState{}
	s.HandleSortKey(columns, "2")
	assert.Equal(t, "name", s.Key, "after pressing 2: Key")

	// Press "3" -> sorts by DATE (3rd sortable column, skipping non-sortable TITLE)
	s = SortState{}
	s.HandleSortKey(columns, "3")
	assert.Equal(t, "date", s.Key, "after pressing 3: Key")

	// Press "4" -> no 4th sortable column, should not change
	s = SortState{}
	assert.False(t, s.HandleSortKey(columns, "4"), "HandleSortKey('4') should return false (no 4th sortable column)")

	// F-keys work too
	s = SortState{}
	s.HandleSortKey(columns, "f2")
	assert.Equal(t, "name", s.Key, "after pressing f2: Key")

	// Non-sort key returns false
	s = SortState{}
	assert.False(t, s.HandleSortKey(columns, "q"), "HandleSortKey('q') should return false")
}

func TestSortableColumnIndex(t *testing.T) {
	columns := []Column{
		{Header: "A"},               // Not sortable
		{Header: "B", SortKey: "b"}, // Sortable #1
		{Header: "C"},               // Not sortable
		{Header: "D", SortKey: "d"}, // Sortable #2
	}

	assert.Equal(t, 1, SortableColumnIndex(columns, 1), "SortableColumnIndex(1)")
	assert.Equal(t, 3, SortableColumnIndex(columns, 2), "SortableColumnIndex(2)")
	assert.Equal(t, -1, SortableColumnIndex(columns, 3), "SortableColumnIndex(3)")
}

func TestSortableColumnsHelp(t *testing.T) {
	columns := []Column{
		{Header: "", Width: 2},
		{Header: "ID", SortKey: "id"},
		{Header: "NAME", SortKey: "name"},
	}

	lines := SortableColumnsHelp(columns)
	require.Len(t, lines, 3, "expected 3 help lines") // 2 columns + toggle hint
	assert.Contains(t, lines[0], "1/F1", "first line should mention 1/F1")
	assert.Contains(t, lines[0], "ID", "first line should mention ID")
	assert.Contains(t, lines[1], "2/F2", "second line should mention 2/F2")
	assert.Contains(t, lines[1], "NAME", "second line should mention NAME")

	// No sortable columns
	noSort := []Column{{Header: "A"}, {Header: "B"}}
	assert.Len(t, SortableColumnsHelp(noSort), 0, "expected 0 help lines for no sortable columns")
}

func TestParseSortKeyNumber(t *testing.T) {
	tests := []struct {
		key  string
		want int
	}{
		{"1", 1}, {"2", 2}, {"9", 9},
		{"f1", 1}, {"f5", 5}, {"f9", 9},
		{"q", 0}, {"enter", 0}, {"", 0},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, ParseSortKeyNumber(tt.key), "ParseSortKeyNumber(%q)", tt.key)
	}
}

func TestFormatCell(t *testing.T) {
	tests := []struct {
		name  string
		value string
		col   Column
		width int
		want  string
	}{
		{
			name:  "left align",
			value: "hi",
			col:   Column{Align: AlignLeft},
			width: 5,
			want:  "hi   ",
		},
		{
			name:  "right align",
			value: "hi",
			col:   Column{Align: AlignRight},
			width: 5,
			want:  "   hi",
		},
		{
			name:  "center align",
			value: "hi",
			col:   Column{Align: AlignCenter},
			width: 6,
			want:  "  hi  ",
		},
		{
			name:  "truncate",
			value: "hello world",
			col:   Column{Truncate: true},
			width: 5,
			want:  "hell…",
		},
		{
			name:  "truncate from start",
			value: "/home/user/projects/myapp",
			col:   Column{Truncate: true, TruncateMode: TruncateStart},
			width: 15,
			want:  "…projects/myapp",
		},
		{
			name:  "truncate from start padded",
			value: "/home/user",
			col:   Column{Truncate: true, TruncateMode: TruncateStart},
			width: 15,
			want:  "/home/user     ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatCell(tt.value, tt.col, tt.width)
			assert.Equal(t, tt.want, got, "FormatCell(%q, ..., %d)", tt.value, tt.width)
		})
	}
}
