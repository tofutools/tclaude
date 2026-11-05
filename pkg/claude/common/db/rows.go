package db

import "database/sql"

// rowQuerier is what queryRows runs a query on: *sql.DB, *sql.Tx and
// *sql.Conn all satisfy it.
type rowQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// queryRows runs query on q and returns every row as scanned by scan. It is
// the one place this package iterates rows for a plain list, so every list
// read closes its rows and reports the error that ended iteration
// (Rows.Err), never a silently truncated list.
//
// A query with no rows returns a nil slice, as `var out []T` did. A query or
// scan error returns (nil, err). An error that ends iteration returns the
// rows read so far with that error, as `return out, rows.Err()` did.
func queryRows[T any](q rowQuerier, scan func(rowScanner) (T, error), query string, args ...any) ([]T, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, scan)
}

// collectRows is queryRows for rows a caller already has. It closes rows.
func collectRows[T any](rows *sql.Rows, scan func(rowScanner) (T, error)) ([]T, error) {
	defer func() { _ = rows.Close() }()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// scanString scans a single text column.
func scanString(s rowScanner) (string, error) {
	var v string
	err := s.Scan(&v)
	return v, err
}
