package db

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQueryRows(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`CREATE TABLE rows_helper_test (v TEXT NOT NULL)`)
	require.NoError(t, err)

	t.Run("no rows is a nil slice", func(t *testing.T) {
		got, err := queryRows(d, scanString, `SELECT v FROM rows_helper_test`)
		require.NoError(t, err)
		require.Nil(t, got)
	})

	_, err = d.Exec(`INSERT INTO rows_helper_test (v) VALUES ('a'), ('b'), ('c')`)
	require.NoError(t, err)

	t.Run("rows in query order", func(t *testing.T) {
		got, err := queryRows(d, scanString, `SELECT v FROM rows_helper_test WHERE v <> ? ORDER BY v DESC`, "b")
		require.NoError(t, err)
		require.Equal(t, []string{"c", "a"}, got)
	})

	t.Run("a query error returns nil", func(t *testing.T) {
		got, err := queryRows(d, scanString, `SELECT nope FROM rows_helper_test`)
		require.Error(t, err)
		require.Nil(t, got)
	})

	t.Run("a scan error returns nil and stops", func(t *testing.T) {
		boom := errors.New("boom")
		calls := 0
		got, err := queryRows(d, func(s rowScanner) (string, error) {
			calls++
			v, err := scanString(s)
			if err == nil && v == "b" {
				return "", boom
			}
			return v, err
		}, `SELECT v FROM rows_helper_test ORDER BY v`)
		require.ErrorIs(t, err, boom)
		require.Nil(t, got)
		require.Equal(t, 2, calls, "no row is scanned after the failing one")
	})
}
