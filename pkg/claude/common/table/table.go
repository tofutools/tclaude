package table

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// SortDirection for column sorting
type SortDirection int

const (
	SortNone SortDirection = iota
	SortAsc
	SortDesc
)

// SortConfig specifies current sort state
type SortConfig struct {
	Column    int           // Column index to sort by (-1 = none)
	Direction SortDirection // Sort direction
}

// Row represents a table row
type Row struct {
	Cells []string       // Cell contents (one per column)
	Style lipgloss.Style // Optional row-level style override
}

// Table holds table state and configuration
type Table struct {
	Columns        []Column
	Rows           []Row
	SelectedIndex  int // Currently selected row (-1 for none)
	ViewportOffset int // First visible row index
	ViewportHeight int // Number of visible rows (0 = show all)
	TerminalWidth  int // Terminal width for width calculations
	Sort           SortConfig
	Padding        int // Padding between columns (default 2)

	// Styles
	HeaderStyle   lipgloss.Style
	SelectedStyle lipgloss.Style
	SeparatorChar string // Default "─"

	// Cached calculated widths
	calculatedWidths []int
}

// New creates a new Table with the given columns
func New(columns ...Column) *Table {
	return &Table{
		Columns:        columns,
		SelectedIndex:  -1,
		ViewportOffset: 0,
		ViewportHeight: 0,
		TerminalWidth:  DefaultTerminalWidth,
		Sort:           SortConfig{Column: -1, Direction: SortNone},
		Padding:        2,
		HeaderStyle:    lipgloss.NewStyle(),
		SelectedStyle:  lipgloss.NewStyle().Reverse(true),
		SeparatorChar:  "─",
	}
}

// SetTerminalWidth sets the terminal width and invalidates cached widths
func (t *Table) SetTerminalWidth(width int) {
	if width != t.TerminalWidth {
		t.TerminalWidth = width
		t.calculatedWidths = nil
	}
}

// AddRow adds a row to the table and invalidates cached widths
func (t *Table) AddRow(row Row) {
	t.Rows = append(t.Rows, row)
	t.calculatedWidths = nil // Content changed, recalculate widths
}

// ClearRows removes all rows from the table and invalidates cached widths
func (t *Table) ClearRows() {
	t.Rows = nil
	t.calculatedWidths = nil
}

// CalculateWidths computes actual column widths based on terminal width.
// For flexible columns, widths are capped at actual content width (no wasted space).
func (t *Table) CalculateWidths() []int {
	if t.calculatedWidths == nil {
		contentWidths := t.maxContentWidths()
		t.calculatedWidths = CalculateColumnWidthsWithContent(t.Columns, t.TerminalWidth, t.Padding, contentWidths)
	}
	return t.calculatedWidths
}

// maxContentWidths returns the maximum content width for each column based on rows.
// Returns nil if no rows exist.
func (t *Table) maxContentWidths() []int {
	if len(t.Rows) == 0 {
		return nil
	}

	widths := make([]int, len(t.Columns))

	// Include header widths
	for i, col := range t.Columns {
		widths[i] = StringWidth(col.Header)
	}

	// Find max content width per column
	for _, row := range t.Rows {
		for i, cell := range row.Cells {
			if i < len(widths) {
				w := StringWidth(cell)
				if w > widths[i] {
					widths[i] = w
				}
			}
		}
	}

	return widths
}

// InvalidateWidths forces recalculation of column widths on next render
func (t *Table) InvalidateWidths() {
	t.calculatedWidths = nil
}

// GetColumnWidth returns the calculated width for a specific column index.
// Useful for pre-truncating content before adding rows.
func (t *Table) GetColumnWidth(index int) int {
	widths := t.CalculateWidths()
	if index < 0 || index >= len(widths) {
		return 0
	}
	return widths[index]
}

// RenderHeader returns the formatted header row
func (t *Table) RenderHeader() string {
	widths := t.CalculateWidths()
	if len(widths) == 0 {
		return ""
	}

	parts := make([]string, len(t.Columns))
	for i, col := range t.Columns {
		header := col.Header

		// Add sort indicator if this column is sorted
		if t.Sort.Column == i {
			switch t.Sort.Direction {
			case SortAsc:
				header += " ▲"
			case SortDesc:
				header += " ▼"
			}
		}

		parts[i] = FormatCell(header, col, widths[i])
	}

	separator := strings.Repeat(" ", t.Padding)
	line := strings.Join(parts, separator)
	return t.HeaderStyle.Render(line)
}

// RenderSeparator returns the separator line between header and rows
func (t *Table) RenderSeparator() string {
	widths := t.CalculateWidths()
	if len(widths) == 0 {
		return ""
	}

	totalWidth := 0
	for _, w := range widths {
		totalWidth += w
	}
	totalWidth += t.Padding * (len(widths) - 1)

	return strings.Repeat(t.SeparatorChar, totalWidth)
}

// VisibleRows returns the slice of rows currently visible in the viewport
func (t *Table) VisibleRows() []Row {
	if len(t.Rows) == 0 {
		return nil
	}

	start := max(t.ViewportOffset, 0)
	if start >= len(t.Rows) {
		start = len(t.Rows) - 1
	}

	end := len(t.Rows)
	if t.ViewportHeight > 0 {
		end = min(start+t.ViewportHeight, len(t.Rows))
	}

	return t.Rows[start:end]
}

// RenderRows returns visible rows as formatted string
func (t *Table) RenderRows() string {
	widths := t.CalculateWidths()
	if len(widths) == 0 || len(t.Rows) == 0 {
		return ""
	}

	visibleRows := t.VisibleRows()
	lines := make([]string, 0, len(visibleRows))
	separator := strings.Repeat(" ", t.Padding)

	for i, row := range visibleRows {
		actualIndex := t.ViewportOffset + i

		parts := make([]string, len(t.Columns))
		for j, col := range t.Columns {
			cell := ""
			if j < len(row.Cells) {
				cell = row.Cells[j]
			}
			parts[j] = FormatCell(cell, col, widths[j])
		}

		line := strings.Join(parts, separator)

		// Apply styling - combine row style with selection style to preserve row colors
		if actualIndex == t.SelectedIndex {
			// Combine row foreground (if any) with selection background/bold
			combinedStyle := t.SelectedStyle.Inherit(row.Style)
			line = combinedStyle.Render(line)
		} else {
			line = row.Style.Render(line)
		}

		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}

// NeedsScrollIndicator returns true if viewport scrolling is active
func (t *Table) NeedsScrollIndicator() bool {
	return t.ViewportHeight > 0 && len(t.Rows) > t.ViewportHeight
}

// ScrollPercentage returns current scroll position as percentage (0-100)
func (t *Table) ScrollPercentage() int {
	if !t.NeedsScrollIndicator() {
		return 100
	}

	maxOffset := len(t.Rows) - t.ViewportHeight
	if maxOffset <= 0 {
		return 100
	}

	return (t.ViewportOffset * 100) / maxOffset
}

// RenderScrollIndicator returns scroll percentage indicator
func (t *Table) RenderScrollIndicator(style lipgloss.Style) string {
	if !t.NeedsScrollIndicator() {
		return ""
	}
	return style.Render(fmt.Sprintf(" %d%% ", t.ScrollPercentage()))
}

// EnsureCursorVisible adjusts viewport to keep selected row visible
func (t *Table) EnsureCursorVisible() {
	if t.SelectedIndex < 0 || t.ViewportHeight <= 0 {
		return
	}

	// If selection is above viewport, scroll up
	if t.SelectedIndex < t.ViewportOffset {
		t.ViewportOffset = t.SelectedIndex
	}

	// If selection is below viewport, scroll down
	if t.SelectedIndex >= t.ViewportOffset+t.ViewportHeight {
		t.ViewportOffset = t.SelectedIndex - t.ViewportHeight + 1
	}

	// Clamp viewport offset
	maxOffset := max(len(t.Rows)-t.ViewportHeight, 0)
	t.ViewportOffset = max(min(t.ViewportOffset, maxOffset), 0)
}

// MoveSelection moves the selection by delta rows (positive = down, negative = up)
func (t *Table) MoveSelection(delta int) {
	if len(t.Rows) == 0 {
		return
	}

	t.SelectedIndex += delta

	// Clamp to valid range
	if t.SelectedIndex < 0 {
		t.SelectedIndex = 0
	}
	if t.SelectedIndex >= len(t.Rows) {
		t.SelectedIndex = len(t.Rows) - 1
	}

	t.EnsureCursorVisible()
}

// Render returns the complete table as a string (header, separator, rows)
func (t *Table) Render() string {
	return t.RenderWithScroll(nil)
}

// RenderWithScroll returns the complete table with optional scroll indicator.
// Pass a style for the scroll indicator, or nil to omit it.
func (t *Table) RenderWithScroll(scrollStyle *lipgloss.Style) string {
	var parts []string

	header := t.RenderHeader()
	if header != "" {
		parts = append(parts, header)
	}

	sep := t.RenderSeparator()
	if sep != "" {
		parts = append(parts, sep)
	}

	rows := t.RenderRows()
	if rows != "" {
		parts = append(parts, rows)
	}

	if scrollStyle != nil && t.NeedsScrollIndicator() {
		parts = append(parts, t.RenderScrollIndicator(*scrollStyle))
	}

	return strings.Join(parts, "\n")
}

// RowCount returns the total number of rows
func (t *Table) RowCount() int {
	return len(t.Rows)
}

// SelectedRow returns the currently selected row, or nil if none selected
func (t *Table) SelectedRow() *Row {
	if t.SelectedIndex < 0 || t.SelectedIndex >= len(t.Rows) {
		return nil
	}
	return &t.Rows[t.SelectedIndex]
}
