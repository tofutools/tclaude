package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// SchemaSQL returns the canonical SQLite CREATE statements for the database —
// the `.schema` equivalent: every table / index / view / trigger we define,
// ordered deterministically so the output is a stable, diffable artifact.
// Foreign-key REFERENCES clauses show inline in each CREATE. Auto-created
// sqlite_* objects (e.g. sqlite_sequence, sqlite_autoindex_*) are excluded.
//
// Used by `tclaude db schema` (against the live DB) and by the golden
// schema-snapshot test (against a fresh fully-migrated DB).
func SchemaSQL(d *sql.DB) (string, error) {
	rows, err := d.Query(`
		SELECT sql FROM sqlite_master
		WHERE type IN ('table','index','view','trigger')
		  AND name NOT LIKE 'sqlite_%'
		  AND sql IS NOT NULL
		ORDER BY type, name`)
	if err != nil {
		return "", fmt.Errorf("query sqlite_master: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var b strings.Builder
	for rows.Next() {
		var stmt string
		if err := rows.Scan(&stmt); err != nil {
			return "", fmt.Errorf("scan schema row: %w", err)
		}
		// SQLite stores the CREATE text verbatim (minus the trailing
		// semicolon); re-add it and a blank line so multi-line CREATEs
		// stay visually separated in the golden file and in diffs.
		b.WriteString(strings.TrimRight(stmt, "\n"))
		b.WriteString(";\n\n")
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate schema rows: %w", err)
	}
	return b.String(), nil
}

// SchemaColumn is one column's structure, mirroring pragma_table_info.
type SchemaColumn struct {
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	NotNull    bool    `json:"not_null"`
	Default    *string `json:"default,omitempty"`
	PrimaryKey int     `json:"pk"` // 0 = not part of PK; 1-based position otherwise
	// Identity classifies the column for the conv-id -> agent_id FK audit
	// ("conv", "agent", or "" when the name signals neither).
	Identity string `json:"identity,omitempty"`
}

// SchemaForeignKey is one declared foreign key, mirroring
// pragma_foreign_key_list.
type SchemaForeignKey struct {
	Column    string `json:"column"`     // local column ("from")
	RefTable  string `json:"ref_table"`  // referenced table
	RefColumn string `json:"ref_column"` // referenced column ("to")
	OnUpdate  string `json:"on_update,omitempty"`
	OnDelete  string `json:"on_delete,omitempty"`
}

// SchemaTable is one table's structured form.
type SchemaTable struct {
	Name        string             `json:"name"`
	Columns     []SchemaColumn     `json:"columns"`
	ForeignKeys []SchemaForeignKey `json:"foreign_keys,omitempty"`
}

// SchemaInfo is the structured (--json) form of the whole schema.
type SchemaInfo struct {
	SchemaVersion int           `json:"schema_version"`
	Tables        []SchemaTable `json:"tables"`
}

// SchemaStructured returns the per-table column + foreign-key structure of the
// database, derived from pragma_table_info / pragma_foreign_key_list. Columns
// carry an Identity classification ("conv" / "agent") so conv-keyed columns can
// be flagged programmatically during the identity FK audit.
func SchemaStructured(d *sql.DB) (*SchemaInfo, error) {
	info := &SchemaInfo{Tables: []SchemaTable{}}

	// schema_version is informative, not load-bearing — tolerate its absence
	// (e.g. a partial-schema heal DB) rather than failing the whole dump.
	_ = d.QueryRow("SELECT version FROM schema_version").Scan(&info.SchemaVersion)

	names, err := schemaTableNames(d)
	if err != nil {
		return nil, err
	}

	for _, name := range names {
		t := SchemaTable{Name: name}

		cols, err := schemaTableColumns(d, name)
		if err != nil {
			return nil, fmt.Errorf("table_info(%s): %w", name, err)
		}
		t.Columns = cols

		fks, err := schemaForeignKeys(d, name)
		if err != nil {
			return nil, fmt.Errorf("foreign_key_list(%s): %w", name, err)
		}
		t.ForeignKeys = fks

		info.Tables = append(info.Tables, t)
	}
	return info, nil
}

func schemaTableNames(d *sql.DB) ([]string, error) {
	rows, err := d.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

func schemaTableColumns(d *sql.DB, table string) ([]SchemaColumn, error) {
	rows, err := d.Query(`SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var cols []SchemaColumn
	for rows.Next() {
		var (
			c    SchemaColumn
			nn   int
			dflt sql.NullString
		)
		if err := rows.Scan(&c.Name, &c.Type, &nn, &dflt, &c.PrimaryKey); err != nil {
			return nil, err
		}
		c.NotNull = nn != 0
		if dflt.Valid {
			c.Default = &dflt.String
		}
		c.Identity = classifyIdentityColumn(c.Name)
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

func schemaForeignKeys(d *sql.DB, table string) ([]SchemaForeignKey, error) {
	rows, err := d.Query(`SELECT "table", "from", "to", on_update, on_delete FROM pragma_foreign_key_list(?)`, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var fks []SchemaForeignKey
	for rows.Next() {
		var fk SchemaForeignKey
		var refCol sql.NullString // "to" is NULL when the FK targets the ref table's PK
		if err := rows.Scan(&fk.RefTable, &fk.Column, &refCol, &fk.OnUpdate, &fk.OnDelete); err != nil {
			return nil, err
		}
		fk.RefColumn = refCol.String
		fks = append(fks, fk)
	}
	return fks, rows.Err()
}

// classifyIdentityColumn tags a column name for the conv-id -> agent_id FK
// audit. Returns "agent" for stable-actor refs (agent_id, *_agent, *agent_id),
// "conv" for conversation-generation refs (conv_id, *_conv, *conv_id), or ""
// when the name signals neither. agent is checked first so a hypothetical
// agent-flavoured name never falls through to the conv suffixes.
func classifyIdentityColumn(name string) string {
	n := strings.ToLower(name)
	switch {
	case n == "agent_id" || strings.HasSuffix(n, "_agent") || strings.HasSuffix(n, "agent_id"):
		return "agent"
	case n == "conv_id" || strings.HasSuffix(n, "_conv") || strings.HasSuffix(n, "conv_id"):
		return "conv"
	default:
		return ""
	}
}
