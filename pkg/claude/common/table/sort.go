package table

import "fmt"

// SortState tracks current sort settings using a key name.
// Unlike SortConfig (which uses column indices), SortState uses string keys
// so it can persist across table rebuilds and adapt to different column layouts.
type SortState struct {
	Key       string        // SortKey of the sorted column ("" = none)
	Direction SortDirection // Sort direction
}

// Toggle cycles sort for a key: none -> asc -> desc -> none
func (s *SortState) Toggle(key string) {
	if s.Key != key {
		s.Key = key
		s.Direction = SortAsc
	} else if s.Direction == SortAsc {
		s.Direction = SortDesc
	} else {
		s.Key = ""
		s.Direction = SortNone
	}
}

// ToConfig converts key-based SortState to index-based SortConfig for table rendering.
func (s *SortState) ToConfig(columns []Column) SortConfig {
	if s.Key == "" {
		return SortConfig{Column: -1, Direction: SortNone}
	}
	for i, col := range columns {
		if col.SortKey == s.Key {
			return SortConfig{Column: i, Direction: s.Direction}
		}
	}
	return SortConfig{Column: -1, Direction: SortNone}
}

// HandleSortKey processes a keyboard key press (e.g., "1", "f1") and toggles sort
// for the corresponding sortable column. Returns true if sort state changed.
func (s *SortState) HandleSortKey(columns []Column, key string) bool {
	n := ParseSortKeyNumber(key)
	if n <= 0 {
		return false
	}
	idx := SortableColumnIndex(columns, n)
	if idx < 0 {
		return false
	}
	s.Toggle(columns[idx].SortKey)
	return true
}

// ParseSortKeyNumber converts a key string ("1", "f1", etc.) to a 1-based number.
// Returns 0 if the key is not a sort key.
func ParseSortKeyNumber(key string) int {
	switch key {
	case "1", "f1":
		return 1
	case "2", "f2":
		return 2
	case "3", "f3":
		return 3
	case "4", "f4":
		return 4
	case "5", "f5":
		return 5
	case "6", "f6":
		return 6
	case "7", "f7":
		return 7
	case "8", "f8":
		return 8
	case "9", "f9":
		return 9
	default:
		return 0
	}
}

// SortableColumnIndex returns the actual column index for the n-th sortable column (1-based).
// Returns -1 if n is out of range.
func SortableColumnIndex(columns []Column, n int) int {
	count := 0
	for i, col := range columns {
		if col.SortKey != "" {
			count++
			if count == n {
				return i
			}
		}
	}
	return -1
}

// SortableColumnsHelp returns help text lines for sortable columns.
func SortableColumnsHelp(columns []Column) []string {
	var lines []string
	n := 0
	for _, col := range columns {
		if col.SortKey != "" {
			n++
			lines = append(lines, fmt.Sprintf("    %d/F%d      Sort by %s", n, n, col.Header))
		}
	}
	if len(lines) > 0 {
		lines = append(lines, "              (press again to toggle asc/desc/off)")
	}
	return lines
}
