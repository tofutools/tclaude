package db

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// migrateV180toV181 converts every SQLite timestamp from RFC3339Nano TEXT to
// guarded int64 Unix nanoseconds. Empty-string sentinels become NULL; malformed
// and out-of-range non-empty values abort the entire transaction. Tables that
// contain timestamps are rebuilt as STRICT so later writers cannot put text
// back into the columns.
func migrateV180toV181(d *sql.DB) error {
	ctx := context.Background()
	conn, err := d.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate v180→v181 (epoch nanoseconds): connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// foreign_keys is connection-scoped and may only be changed outside a
	// transaction. The rebuild restores every FK target before commit, then a
	// positive foreign_key_check proves the graph is intact.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("migrate v180→v181 (epoch nanoseconds): disable foreign keys: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate v180→v181 (epoch nanoseconds): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	tables, err := timestampTables(ctx, tx)
	if err != nil {
		return fmt.Errorf("migrate v180→v181 (epoch nanoseconds): inventory: %w", err)
	}
	triggers, err := suspendAllTriggers(ctx, tx)
	if err != nil {
		return fmt.Errorf("migrate v180→v181 (epoch nanoseconds): suspend triggers: %w", err)
	}
	for _, table := range tables {
		if err := convertTimestampTable(ctx, tx, table); err != nil {
			return fmt.Errorf("migrate v180→v181 (epoch nanoseconds): %w", err)
		}
	}
	allTimestampColumns := timestampColumnNames(tables)
	for _, triggerSQL := range triggers {
		triggerSQL = rewriteTimestampSentinelPredicates(triggerSQL, allTimestampColumns)
		if _, err := tx.ExecContext(ctx, triggerSQL); err != nil {
			return fmt.Errorf("migrate v180→v181 (epoch nanoseconds): restore trigger: %w", err)
		}
	}

	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign-key check: %w", err)
	}
	if rows.Next() {
		var table string
		var rowID, parent any
		var fk int
		_ = rows.Scan(&table, &rowID, &parent, &fk)
		_ = rows.Close()
		return fmt.Errorf("foreign-key check found violation: table=%s rowid=%v parent=%v fk=%d", table, rowID, parent, fk)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("foreign-key check close: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("foreign-key check rows: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = 181`); err != nil {
		return fmt.Errorf("advance schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

type timestampTable struct {
	name       string
	createSQL  string
	columns    []timestampColumn
	objectsSQL []string
}

type timestampColumn struct {
	name        string
	text        bool
	epochSecond bool
	fileMtime   bool
}

func timestampTables(ctx context.Context, tx *sql.Tx) ([]timestampTable, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name, sql FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	type tableSchema struct{ name, sql string }
	var schemas []tableSchema
	for rows.Next() {
		var schema tableSchema
		if err := rows.Scan(&schema.name, &schema.sql); err != nil {
			return nil, err
		}
		schemas = append(schemas, schema)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	var out []timestampTable
	for _, schema := range schemas {
		pragma := `PRAGMA table_info(` + quoteIdentifier(schema.name) + `)`
		columnRows, err := tx.QueryContext(ctx, pragma)
		if err != nil {
			return nil, fmt.Errorf("%s columns: %w", schema.name, err)
		}
		var columns []timestampColumn
		for columnRows.Next() {
			var cid, notNull, pk int
			var name, declaredType string
			var defaultValue any
			if err := columnRows.Scan(&cid, &name, &declaredType, &notNull, &defaultValue, &pk); err != nil {
				_ = columnRows.Close()
				return nil, fmt.Errorf("%s columns: %w", schema.name, err)
			}
			upperType := strings.ToUpper(strings.TrimSpace(declaredType))
			if upperType == "TEXT" && isTimestampColumn(name) {
				columns = append(columns, timestampColumn{name: name, text: true})
				continue
			}
			if upperType == "INTEGER" && isLegacyEpochSecondColumn(schema.name, name) {
				columns = append(columns, timestampColumn{name: name, epochSecond: true})
				continue
			}
			if upperType == "INTEGER" && schema.name == "conv_index" && name == "file_mtime" {
				columns = append(columns, timestampColumn{name: name, fileMtime: true})
			}
		}
		if err := columnRows.Close(); err != nil {
			return nil, fmt.Errorf("%s columns close: %w", schema.name, err)
		}
		if err := columnRows.Err(); err != nil {
			return nil, fmt.Errorf("%s columns: %w", schema.name, err)
		}
		if len(columns) == 0 {
			continue
		}

		objectRows, err := tx.QueryContext(ctx, `SELECT sql FROM sqlite_master
			WHERE tbl_name = ? AND type = 'index' AND sql IS NOT NULL
			ORDER BY rowid`, schema.name)
		if err != nil {
			return nil, fmt.Errorf("%s dependent objects: %w", schema.name, err)
		}
		var objects []string
		for objectRows.Next() {
			var objectSQL string
			if err := objectRows.Scan(&objectSQL); err != nil {
				_ = objectRows.Close()
				return nil, fmt.Errorf("%s dependent objects: %w", schema.name, err)
			}
			objects = append(objects, objectSQL)
		}
		if err := objectRows.Close(); err != nil {
			return nil, fmt.Errorf("%s dependent objects close: %w", schema.name, err)
		}
		if err := objectRows.Err(); err != nil {
			return nil, fmt.Errorf("%s dependent objects: %w", schema.name, err)
		}
		out = append(out, timestampTable{name: schema.name, createSQL: schema.sql, columns: columns, objectsSQL: objects})
	}
	return out, nil
}

// Rebuilding one table can temporarily invalidate a trigger owned by another
// table when that trigger references the table being replaced. Suspend the
// complete trigger set transactionally and restore it only after every
// original table name exists again.
func suspendAllTriggers(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name, sql FROM sqlite_master
		WHERE type = 'trigger' AND sql IS NOT NULL ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	var names, statements []string
	for rows.Next() {
		var name, statement string
		if err := rows.Scan(&name, &statement); err != nil {
			_ = rows.Close()
			return nil, err
		}
		names = append(names, name)
		statements = append(statements, statement)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, name := range names {
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER `+quoteIdentifier(name)); err != nil {
			return nil, err
		}
	}
	return statements, nil
}

func isTimestampColumn(name string) bool {
	name = strings.ToLower(name)
	return name == "at" || name == "created" || name == "modified" ||
		name == "first_seen" || name == "last_seen" || name == "last_hook" || name == "healthy_since" || name == "file_mtime" ||
		strings.HasSuffix(name, "_at")
}

func isLegacyEpochSecondColumn(table, column string) bool {
	return (table == "dashboard_session_grace" && column == "expires_at") ||
		(table == "agentd_idempotency" && (column == "created_at" || column == "expires_at"))
}

func convertTimestampTable(ctx context.Context, tx *sql.Tx, table timestampTable) error {
	for _, column := range table.columns {
		if err := convertTimestampColumn(ctx, tx, table.name, column); err != nil {
			return err
		}
	}

	tempName := "__tclaude_v181_" + table.name
	columnSet := make(map[string]bool, len(table.columns))
	for _, column := range table.columns {
		if column.text || column.fileMtime {
			columnSet[column.name] = true
		}
	}
	createSQL := rewriteTimestampSentinelPredicates(table.createSQL, timestampColumnNames([]timestampTable{table}))
	createSQL, err := rewriteTimestampTableSQL(createSQL, tempName, columnSet)
	if err != nil {
		return fmt.Errorf("%s rewrite schema: %w", table.name, err)
	}
	if _, err := tx.ExecContext(ctx, createSQL); err != nil {
		return fmt.Errorf("%s create STRICT replacement: %w", table.name, err)
	}
	selectList, err := timestampCopySelectList(ctx, tx, table)
	if err != nil {
		return fmt.Errorf("%s build copy projection: %w", table.name, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+quoteIdentifier(tempName)+` SELECT `+selectList+` FROM `+quoteIdentifier(table.name)); err != nil {
		return fmt.Errorf("%s copy rows into STRICT replacement: %w", table.name, err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE `+quoteIdentifier(table.name)); err != nil {
		return fmt.Errorf("%s drop legacy table: %w", table.name, err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE `+quoteIdentifier(tempName)+` RENAME TO `+quoteIdentifier(table.name)); err != nil {
		return fmt.Errorf("%s install STRICT replacement: %w", table.name, err)
	}
	for _, objectSQL := range table.objectsSQL {
		objectSQL = rewriteTimestampSentinelPredicates(objectSQL, timestampColumnNames([]timestampTable{table}))
		if _, err := tx.ExecContext(ctx, objectSQL); err != nil {
			return fmt.Errorf("%s recreate dependent object: %w", table.name, err)
		}
	}
	return nil
}

func timestampColumnNames(tables []timestampTable) []string {
	seen := make(map[string]bool)
	var names []string
	for _, table := range tables {
		for _, column := range table.columns {
			if !seen[column.name] {
				seen[column.name] = true
				names = append(names, column.name)
			}
		}
	}
	return names
}

// rewriteTimestampSentinelPredicates updates schema objects that outlive a
// table rebuild. In particular partial indexes and triggers can carry the old
// empty-string absence contract even after their column becomes INTEGER.
func rewriteTimestampSentinelPredicates(statement string, columns []string) string {
	for _, column := range columns {
		identifier := `(?:(?:"[^"]+"|[A-Za-z_][A-Za-z0-9_]*)\.)?(?:"` + regexp.QuoteMeta(column) + `"|\b` + regexp.QuoteMeta(column) + `\b)`
		for _, rewrite := range []struct {
			pattern, replacement string
		}{
			{`(?i)(` + identifier + `)\s*=\s*''`, `${1} IS NULL`},
			{`(?i)''\s*=\s*(` + identifier + `)`, `${1} IS NULL`},
			{`(?i)(` + identifier + `)\s*(?:!=|<>)\s*''`, `${1} IS NOT NULL`},
			{`(?i)''\s*(?:!=|<>)\s*(` + identifier + `)`, `${1} IS NOT NULL`},
		} {
			statement = regexp.MustCompile(rewrite.pattern).ReplaceAllString(statement, rewrite.replacement)
		}
	}
	return statement
}

func timestampCopySelectList(ctx context.Context, tx *sql.Tx, table timestampTable) (string, error) {
	kinds := make(map[string]timestampColumn, len(table.columns))
	for _, column := range table.columns {
		kinds[column.name] = column
	}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table.name)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var expressions []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", err
		}
		quoted := quoteIdentifier(name)
		column := kinds[name]
		switch {
		case column.text:
			expressions = append(expressions, `NULLIF(`+quoted+`, '')`)
		case column.fileMtime:
			expressions = append(expressions, `NULLIF(`+quoted+`, 0)`)
		default:
			expressions = append(expressions, quoted)
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return strings.Join(expressions, ", "), nil
}

func convertTimestampColumn(ctx context.Context, tx *sql.Tx, table string, column timestampColumn) error {
	query := `SELECT rowid, ` + quoteIdentifier(column.name) + ` FROM ` + quoteIdentifier(table)
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("%s.%s read: %w", table, column.name, err)
	}
	type conversion struct {
		rowID int64
		value any
	}
	var updates []conversion
	for rows.Next() {
		var rowID int64
		var raw any
		if err := rows.Scan(&rowID, &raw); err != nil {
			_ = rows.Close()
			return fmt.Errorf("%s.%s scan: %w", table, column.name, err)
		}
		if raw == nil {
			continue
		}
		if column.epochSecond {
			seconds, ok := raw.(int64)
			if !ok {
				return fmt.Errorf("%s.%s rowid %d: expected INTEGER seconds, got %T", table, column.name, rowID, raw)
			}
			ns, err := timeToUnixNano(timeFromUnixSeconds(seconds))
			if err != nil {
				return fmt.Errorf("%s.%s rowid %d: %w", table, column.name, rowID, err)
			}
			updates = append(updates, conversion{rowID: rowID, value: ns})
			continue
		}
		if column.fileMtime {
			legacyValue, ok := raw.(int64)
			if !ok {
				return fmt.Errorf("%s.%s rowid %d: expected INTEGER seconds or milliseconds, got %T", table, column.name, rowID, raw)
			}
			if legacyValue == 0 {
				continue
			}
			ns, err := legacyFileMtimeToUnixNano(legacyValue)
			if err != nil {
				return fmt.Errorf("%s.%s rowid %d value %d: %w", table, column.name, rowID, legacyValue, err)
			}
			updates = append(updates, conversion{rowID: rowID, value: ns})
			continue
		}
		var textValue string
		switch value := raw.(type) {
		case string:
			textValue = value
		case []byte:
			textValue = string(value)
		default:
			return fmt.Errorf("%s.%s rowid %d: expected TEXT timestamp, got %T", table, column.name, rowID, raw)
		}
		if textValue == "" {
			continue
		}
		parsed, err := parseLegacyDBTime(textValue)
		if err != nil {
			return fmt.Errorf("%s.%s rowid %d value %q: %w", table, column.name, rowID, textValue, err)
		}
		ns, err := timeToUnixNano(parsed)
		if err != nil {
			return fmt.Errorf("%s.%s rowid %d value %q: %w", table, column.name, rowID, textValue, err)
		}
		updates = append(updates, conversion{rowID: rowID, value: ns})
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("%s.%s close: %w", table, column.name, err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s.%s rows: %w", table, column.name, err)
	}
	updateSQL := `UPDATE ` + quoteIdentifier(table) + ` SET ` + quoteIdentifier(column.name) + ` = ? WHERE rowid = ?`
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx, updateSQL, update.value, update.rowID); err != nil {
			return fmt.Errorf("%s.%s rowid %d update: %w", table, column.name, update.rowID, err)
		}
	}
	return nil
}

func timeFromUnixSeconds(seconds int64) time.Time {
	return time.Unix(seconds, 0).UTC()
}

// Two historical copy paths wrote current filesystem mtimes in milliseconds
// even though conv_index.file_mtime was documented as seconds. Values in the
// modern millisecond band (2000-01-01 through the Unix-nanosecond ceiling) are
// repaired as milliseconds; smaller values are seconds. This narrow threshold
// avoids silently reinterpreting ordinary out-of-range second fixtures as a
// different unit. Values accepted by neither arm abort with row context.
func legacyFileMtimeToUnixNano(value int64) (int64, error) {
	const (
		modernMillisFloor = int64(946684800000)
		maxUnixNanoMillis = int64(^uint64(0)>>1) / int64(time.Millisecond)
	)
	if value >= modernMillisFloor && value <= maxUnixNanoMillis {
		return timeToUnixNano(time.UnixMilli(value))
	}
	return timeToUnixNano(timeFromUnixSeconds(value))
}

var timestampNotNull = regexp.MustCompile(`(?i)\s+NOT\s+NULL`)
var emptyTimestampDefault = regexp.MustCompile(`(?i)\s+DEFAULT\s+''`)
var zeroTimestampDefault = regexp.MustCompile(`(?i)\s+NOT\s+NULL\s+DEFAULT\s+0`)

func rewriteTimestampTableSQL(createSQL, tempName string, timestampColumns map[string]bool) (string, error) {
	open := strings.Index(createSQL, "(")
	close := strings.LastIndex(createSQL, ")")
	if open < 0 || close <= open {
		return "", fmt.Errorf("malformed CREATE TABLE SQL")
	}
	body := createSQL[open+1 : close]
	definitions, err := splitTopLevelDefinitions(body)
	if err != nil {
		return "", err
	}
	for i, definition := range definitions {
		definitions[i] = rewriteTimestampDefinition(definition, timestampColumns)
	}
	suffix := strings.TrimSpace(createSQL[close+1:])
	if !strings.Contains(strings.ToUpper(suffix), "STRICT") {
		if suffix != "" {
			suffix += " "
		}
		suffix += "STRICT"
	}
	return `CREATE TABLE ` + quoteIdentifier(tempName) + ` (` + strings.Join(definitions, ",") + `) ` + suffix, nil
}

func rewriteTimestampDefinition(definition string, timestampColumns map[string]bool) string {
	trimmed := strings.TrimLeft(definition, " \t\r\n")
	indent := definition[:len(definition)-len(trimmed)]
	if trimmed == "" {
		return definition
	}
	end := 0
	if trimmed[0] == '"' {
		end = 1
		for end < len(trimmed) {
			if trimmed[end] == '"' {
				end++
				break
			}
			end++
		}
	} else {
		for end < len(trimmed) && !strings.ContainsRune(" \t\r\n", rune(trimmed[end])) {
			end++
		}
	}
	name := strings.Trim(trimmed[:end], `"`)
	if !timestampColumns[name] {
		return definition
	}
	typeStart := end
	for typeStart < len(trimmed) && strings.ContainsRune(" \t\r\n", rune(trimmed[typeStart])) {
		typeStart++
	}
	typeEnd := typeStart
	for typeEnd < len(trimmed) && !strings.ContainsRune(" \t\r\n,", rune(trimmed[typeEnd])) {
		typeEnd++
	}
	columnType := trimmed[typeStart:typeEnd]
	if strings.EqualFold(columnType, "INTEGER") && name == "file_mtime" {
		return indent + zeroTimestampDefault.ReplaceAllString(trimmed, "")
	}
	if !strings.EqualFold(columnType, "TEXT") {
		return definition
	}
	rewritten := trimmed[:typeStart] + "INTEGER" + trimmed[typeEnd:]
	rewritten = timestampNotNull.ReplaceAllString(rewritten, "")
	rewritten = emptyTimestampDefault.ReplaceAllString(rewritten, "")
	return indent + rewritten
}

func splitTopLevelDefinitions(body string) ([]string, error) {
	var out []string
	start, depth := 0, 0
	var quote rune
	for i, r := range body {
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"', '`':
			quote = r
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("unbalanced table definition")
			}
		case ',':
			if depth == 0 {
				out = append(out, body[start:i])
				start = i + 1
			}
		}
	}
	if quote != 0 || depth != 0 {
		return nil, fmt.Errorf("unterminated quote or parentheses in table definition")
	}
	out = append(out, body[start:])
	return out, nil
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
