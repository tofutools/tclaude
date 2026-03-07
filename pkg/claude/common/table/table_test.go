package table

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
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
			if got != tt.want {
				t.Errorf("TruncateWithEllipsis(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
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
			if got != tt.want {
				t.Errorf("ShortenPath(%q, %d) = %q, want %q", tt.path, tt.maxLen, got, tt.want)
			}
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
			if got != tt.want {
				t.Errorf("TruncateFromStart(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
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
			{"⚡", 2, "⚡"},        // Exactly fits
			{"⚡", 3, "⚡ "},       // Needs 1 space padding
			{"⚡", 4, "⚡  "},      // Needs 2 spaces padding
			{" ▷", 2, " ▷"},       // Narrow chars fit
			// ANSI styled text - escape codes should not affect width
			{lipgloss.NewStyle().Foreground(lipgloss.Color("46")).Render("⚡"), 2,
				lipgloss.NewStyle().Foreground(lipgloss.Color("46")).Render("⚡")}, // Styled emoji fits in 2
		}
		for _, tt := range tests {
			got := PadRight(tt.input, tt.width)
			if got != tt.want {
				t.Errorf("PadRight(%q, %d) = %q, want %q", tt.input, tt.width, got, tt.want)
			}
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
			if got != tt.want {
				t.Errorf("PadLeft(%q, %d) = %q, want %q", tt.input, tt.width, got, tt.want)
			}
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
			if got != tt.want {
				t.Errorf("PadCenter(%q, %d) = %q, want %q", tt.input, tt.width, got, tt.want)
			}
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
			if len(got) != len(tt.want) {
				t.Errorf("CalculateColumnWidths() length = %d, want %d", len(got), len(tt.want))
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("CalculateColumnWidths()[%d] = %d, want %d", i, got[i], tt.want[i])
				}
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
	if widths[1] > 20 {
		t.Errorf("PATH column width = %d, expected <= 20 (content-aware)", widths[1])
	}
	if widths[1] < 14 {
		t.Errorf("PATH column width = %d, expected >= 14 (content width)", widths[1])
	}
}

func TestTableNew(t *testing.T) {
	tbl := New(
		Column{Header: "ID", Width: 10},
		Column{Header: "Name", MinWidth: 20},
	)

	if len(tbl.Columns) != 2 {
		t.Errorf("expected 2 columns, got %d", len(tbl.Columns))
	}
	if tbl.SelectedIndex != -1 {
		t.Errorf("expected SelectedIndex -1, got %d", tbl.SelectedIndex)
	}
	if tbl.Padding != 2 {
		t.Errorf("expected Padding 2, got %d", tbl.Padding)
	}
}

func TestTableAddRow(t *testing.T) {
	tbl := New(Column{Header: "A"}, Column{Header: "B"})

	tbl.AddRow(Row{Cells: []string{"1", "2"}})
	tbl.AddRow(Row{Cells: []string{"3", "4"}})

	if len(tbl.Rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(tbl.Rows))
	}

	tbl.ClearRows()
	if len(tbl.Rows) != 0 {
		t.Errorf("expected 0 rows after clear, got %d", len(tbl.Rows))
	}
}

func TestTableRenderHeader(t *testing.T) {
	tbl := New(
		Column{Header: "ID", Width: 5},
		Column{Header: "NAME", Width: 10},
	)
	tbl.SetTerminalWidth(80)

	header := tbl.RenderHeader()

	if !strings.Contains(header, "ID") {
		t.Error("header should contain 'ID'")
	}
	if !strings.Contains(header, "NAME") {
		t.Error("header should contain 'NAME'")
	}
}

func TestTableRenderHeaderWithSort(t *testing.T) {
	tbl := New(
		Column{Header: "ID", Width: 5},
		Column{Header: "NAME", Width: 10},
	)
	tbl.SetTerminalWidth(80)
	tbl.Sort = SortConfig{Column: 1, Direction: SortDesc}

	header := tbl.RenderHeader()

	if !strings.Contains(header, "▼") {
		t.Error("header should contain sort indicator ▼")
	}
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
	if displayWidth != 17 {
		t.Errorf("separator display width = %d, want 17", displayWidth)
	}
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

	if !strings.Contains(rows, "1") {
		t.Error("rows should contain '1'")
	}
	if !strings.Contains(rows, "Alice") {
		t.Error("rows should contain 'Alice'")
	}
	if !strings.Contains(rows, "Bob") {
		t.Error("rows should contain 'Bob'")
	}
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
	if len(visible) != 3 {
		t.Errorf("expected 3 visible rows, got %d", len(visible))
	}
	if visible[0].Cells[0] != "A" {
		t.Errorf("first visible row should be 'A', got %q", visible[0].Cells[0])
	}

	tbl.ViewportOffset = 5
	visible = tbl.VisibleRows()
	if visible[0].Cells[0] != "F" {
		t.Errorf("first visible row should be 'F', got %q", visible[0].Cells[0])
	}
}

func TestTableScrollIndicator(t *testing.T) {
	tbl := New(Column{Header: "ID", Width: 5})
	tbl.SetTerminalWidth(80)

	for range 10 {
		tbl.AddRow(Row{Cells: []string{"x"}})
	}

	// No viewport = no scroll indicator
	if tbl.NeedsScrollIndicator() {
		t.Error("should not need scroll indicator without viewport")
	}

	tbl.ViewportHeight = 5
	if !tbl.NeedsScrollIndicator() {
		t.Error("should need scroll indicator with viewport smaller than rows")
	}

	tbl.ViewportOffset = 0
	if tbl.ScrollPercentage() != 0 {
		t.Errorf("scroll at top should be 0%%, got %d%%", tbl.ScrollPercentage())
	}

	tbl.ViewportOffset = 5 // max offset for 10 rows with viewport 5
	if tbl.ScrollPercentage() != 100 {
		t.Errorf("scroll at bottom should be 100%%, got %d%%", tbl.ScrollPercentage())
	}
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
	if tbl.ViewportOffset != 3 {
		t.Errorf("viewport offset should be 3, got %d", tbl.ViewportOffset)
	}

	// Move selection up
	tbl.SelectedIndex = 1
	tbl.EnsureCursorVisible()
	if tbl.ViewportOffset != 1 {
		t.Errorf("viewport offset should be 1, got %d", tbl.ViewportOffset)
	}
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
	if tbl.SelectedIndex != 1 {
		t.Errorf("selection should be 1, got %d", tbl.SelectedIndex)
	}

	tbl.MoveSelection(10) // Should clamp to max
	if tbl.SelectedIndex != 4 {
		t.Errorf("selection should be 4, got %d", tbl.SelectedIndex)
	}

	tbl.MoveSelection(-10) // Should clamp to 0
	if tbl.SelectedIndex != 0 {
		t.Errorf("selection should be 0, got %d", tbl.SelectedIndex)
	}
}

func TestTableSelectedRow(t *testing.T) {
	tbl := New(Column{Header: "ID", Width: 5})
	tbl.AddRow(Row{Cells: []string{"A"}})
	tbl.AddRow(Row{Cells: []string{"B"}})

	if tbl.SelectedRow() != nil {
		t.Error("should return nil when no selection")
	}

	tbl.SelectedIndex = 1
	row := tbl.SelectedRow()
	if row == nil {
		t.Error("should return row when selected")
	}
	if row.Cells[0] != "B" {
		t.Errorf("selected row should be 'B', got %q", row.Cells[0])
	}
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
	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d", len(lines))
	}
}

func TestTableRowStyle(t *testing.T) {
	tbl := New(Column{Header: "ID", Width: 10})
	tbl.SetTerminalWidth(80)

	redStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("red"))
	tbl.AddRow(Row{Cells: []string{"normal"}})
	tbl.AddRow(Row{Cells: []string{"styled"}, Style: redStyle})

	rows := tbl.RenderRows()

	// Both rows should be present
	if !strings.Contains(rows, "normal") {
		t.Error("should contain 'normal'")
	}
	if !strings.Contains(rows, "styled") {
		t.Error("should contain 'styled'")
	}
}

func TestSortStateToggle(t *testing.T) {
	s := SortState{}

	// First toggle: none -> asc
	s.Toggle("id")
	if s.Key != "id" || s.Direction != SortAsc {
		t.Errorf("after first toggle: Key=%q Dir=%v, want id/Asc", s.Key, s.Direction)
	}

	// Second toggle same key: asc -> desc
	s.Toggle("id")
	if s.Key != "id" || s.Direction != SortDesc {
		t.Errorf("after second toggle: Key=%q Dir=%v, want id/Desc", s.Key, s.Direction)
	}

	// Third toggle same key: desc -> none
	s.Toggle("id")
	if s.Key != "" || s.Direction != SortNone {
		t.Errorf("after third toggle: Key=%q Dir=%v, want empty/None", s.Key, s.Direction)
	}

	// Toggle different key while sorted
	s.Toggle("id")
	s.Toggle("name")
	if s.Key != "name" || s.Direction != SortAsc {
		t.Errorf("after switching column: Key=%q Dir=%v, want name/Asc", s.Key, s.Direction)
	}
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
	if cfg.Column != -1 || cfg.Direction != SortNone {
		t.Errorf("empty sort: Column=%d Dir=%v, want -1/None", cfg.Column, cfg.Direction)
	}

	// Sort by name (column index 2)
	s = SortState{Key: "name", Direction: SortDesc}
	cfg = s.ToConfig(columns)
	if cfg.Column != 2 || cfg.Direction != SortDesc {
		t.Errorf("sort by name: Column=%d Dir=%v, want 2/Desc", cfg.Column, cfg.Direction)
	}

	// Sort by unknown key
	s = SortState{Key: "unknown", Direction: SortAsc}
	cfg = s.ToConfig(columns)
	if cfg.Column != -1 {
		t.Errorf("sort by unknown: Column=%d, want -1", cfg.Column)
	}
}

func TestHandleSortKey(t *testing.T) {
	columns := []Column{
		{Header: "", Width: 2},             // Not sortable
		{Header: "ID", Width: 10, SortKey: "id"},       // Sortable #1
		{Header: "NAME", MinWidth: 20, SortKey: "name"}, // Sortable #2
		{Header: "TITLE", MinWidth: 30},    // Not sortable
		{Header: "DATE", Width: 16, SortKey: "date"},    // Sortable #3
	}

	s := SortState{}

	// Press "1" -> sorts by ID (1st sortable column)
	if !s.HandleSortKey(columns, "1") {
		t.Error("HandleSortKey('1') should return true")
	}
	if s.Key != "id" {
		t.Errorf("after pressing 1: Key=%q, want 'id'", s.Key)
	}

	// Press "2" -> sorts by NAME (2nd sortable column)
	s = SortState{}
	s.HandleSortKey(columns, "2")
	if s.Key != "name" {
		t.Errorf("after pressing 2: Key=%q, want 'name'", s.Key)
	}

	// Press "3" -> sorts by DATE (3rd sortable column, skipping non-sortable TITLE)
	s = SortState{}
	s.HandleSortKey(columns, "3")
	if s.Key != "date" {
		t.Errorf("after pressing 3: Key=%q, want 'date'", s.Key)
	}

	// Press "4" -> no 4th sortable column, should not change
	s = SortState{}
	if s.HandleSortKey(columns, "4") {
		t.Error("HandleSortKey('4') should return false (no 4th sortable column)")
	}

	// F-keys work too
	s = SortState{}
	s.HandleSortKey(columns, "f2")
	if s.Key != "name" {
		t.Errorf("after pressing f2: Key=%q, want 'name'", s.Key)
	}

	// Non-sort key returns false
	s = SortState{}
	if s.HandleSortKey(columns, "q") {
		t.Error("HandleSortKey('q') should return false")
	}
}

func TestSortableColumnIndex(t *testing.T) {
	columns := []Column{
		{Header: "A"},                      // Not sortable
		{Header: "B", SortKey: "b"},        // Sortable #1
		{Header: "C"},                      // Not sortable
		{Header: "D", SortKey: "d"},        // Sortable #2
	}

	if idx := SortableColumnIndex(columns, 1); idx != 1 {
		t.Errorf("SortableColumnIndex(1) = %d, want 1", idx)
	}
	if idx := SortableColumnIndex(columns, 2); idx != 3 {
		t.Errorf("SortableColumnIndex(2) = %d, want 3", idx)
	}
	if idx := SortableColumnIndex(columns, 3); idx != -1 {
		t.Errorf("SortableColumnIndex(3) = %d, want -1", idx)
	}
}

func TestSortableColumnsHelp(t *testing.T) {
	columns := []Column{
		{Header: "", Width: 2},
		{Header: "ID", SortKey: "id"},
		{Header: "NAME", SortKey: "name"},
	}

	lines := SortableColumnsHelp(columns)
	if len(lines) != 3 { // 2 columns + toggle hint
		t.Errorf("expected 3 help lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "1/F1") || !strings.Contains(lines[0], "ID") {
		t.Errorf("first line should mention 1/F1 and ID, got: %s", lines[0])
	}
	if !strings.Contains(lines[1], "2/F2") || !strings.Contains(lines[1], "NAME") {
		t.Errorf("second line should mention 2/F2 and NAME, got: %s", lines[1])
	}

	// No sortable columns
	noSort := []Column{{Header: "A"}, {Header: "B"}}
	if lines := SortableColumnsHelp(noSort); len(lines) != 0 {
		t.Errorf("expected 0 help lines for no sortable columns, got %d", len(lines))
	}
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
		if got := ParseSortKeyNumber(tt.key); got != tt.want {
			t.Errorf("ParseSortKeyNumber(%q) = %d, want %d", tt.key, got, tt.want)
		}
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
			if got != tt.want {
				t.Errorf("FormatCell(%q, ..., %d) = %q, want %q", tt.value, tt.width, got, tt.want)
			}
		})
	}
}
