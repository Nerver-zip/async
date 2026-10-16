package providers

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// SessionMeta describes a conversation session in either Codex or Antigravity CLI.
type SessionMeta struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	Title         string `json:"title"`
	Preview       string `json:"preview,omitempty"`
	Workspace     string `json:"workspace"`
	UpdatedAt     int64  `json:"updated_at"` // Unix timestamp in milliseconds
	UpdatedAtISO  string `json:"updated_at_iso"`
	StepCount     int    `json:"step_count"`
	SubagentCount int    `json:"subagent_count,omitempty"`
	Locked        bool   `json:"locked"`
}

// StaleOverwriteError is returned when the destination already has a newer version of the session.
type StaleOverwriteError struct {
	Provider  string
	SessionID string
	SrcMeta   SessionMeta
	DstMeta   SessionMeta
}

func (e *StaleOverwriteError) Error() string {
	return fmt.Sprintf(
		"conflict: destination %s session %s is newer or further ahead (dest updated=%s steps=%d vs incoming updated=%s steps=%d); pass --force to overwrite",
		e.Provider, e.SessionID,
		e.DstMeta.UpdatedAtISO, e.DstMeta.StepCount,
		e.SrcMeta.UpdatedAtISO, e.SrcMeta.StepCount,
	)
}

// IsStaleOverwriteError reports whether err is or wraps *StaleOverwriteError.
func IsStaleOverwriteError(err error) bool {
	var target *StaleOverwriteError
	return errors.As(err, &target)
}

// NotFoundError is returned when no matching session is found.
type NotFoundError struct {
	Provider  string
	ID        string
	Workspace string
}

func (e *NotFoundError) Error() string {
	if e.ID != "" {
		return fmt.Sprintf("no %s session found matching id %q", e.Provider, e.ID)
	}
	return fmt.Sprintf("no %s session found for workspace %q", e.Provider, e.Workspace)
}

// IsNotFoundError reports whether err is or wraps *NotFoundError.
func IsNotFoundError(err error) bool {
	var target *NotFoundError
	return errors.As(err, &target)
}

// TypedValue preserves exact SQLite column types across JSON serialization.
type TypedValue struct {
	Type string `json:"t"` // "null", "int", "float", "text", "blob"
	Int  int64  `json:"i,omitempty"`
	Flt  float64 `json:"f,omitempty"`
	Str  string `json:"s,omitempty"`
}

// ToSQL converts a TypedValue back into a native driver value.
func (tv TypedValue) ToSQL() any {
	switch tv.Type {
	case "null", "":
		return nil
	case "int":
		return tv.Int
	case "float":
		return tv.Flt
	case "text":
		return tv.Str
	case "blob":
		b, err := base64.StdEncoding.DecodeString(tv.Str)
		if err != nil {
			return []byte(tv.Str)
		}
		return b
	default:
		return tv.Str
	}
}

// FromSQL converts a scanned SQLite value into a TypedValue.
func FromSQL(v any) TypedValue {
	switch val := v.(type) {
	case nil:
		return TypedValue{Type: "null"}
	case int64:
		return TypedValue{Type: "int", Int: val}
	case float64:
		return TypedValue{Type: "float", Flt: val}
	case string:
		return TypedValue{Type: "text", Str: val}
	case []byte:
		return TypedValue{Type: "blob", Str: base64.StdEncoding.EncodeToString(val)}
	default:
		return TypedValue{Type: "text", Str: fmt.Sprintf("%v", val)}
	}
}

// TableSlice holds exported schema and rows for a single SQLite table.
type TableSlice struct {
	TableName  string                  `json:"table_name"`
	CreateSQLs []string                `json:"create_sqls,omitempty"`
	Rows       []map[string]TypedValue `json:"rows"`
}

// TableExists checks whether tableName exists in db.
func TableExists(db *sql.DB, tableName string) (bool, error) {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tableName).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ExportTableSlice queries rows from tableName using whereClause + args and captures DDL from sqlite_master.
func ExportTableSlice(db *sql.DB, tableName, whereClause string, args ...any) (*TableSlice, error) {
	exists, err := TableExists(db, tableName)
	if err != nil || !exists {
		return nil, err
	}

	slice := &TableSlice{
		TableName: tableName,
	}

	ddlRows, err := db.Query(`SELECT sql FROM sqlite_master WHERE tbl_name = ? AND sql IS NOT NULL ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'index' THEN 1 ELSE 2 END`, tableName)
	if err == nil {
		for ddlRows.Next() {
			var ddl string
			if err := ddlRows.Scan(&ddl); err == nil && strings.TrimSpace(ddl) != "" {
				slice.CreateSQLs = append(slice.CreateSQLs, ddl)
			}
		}
		ddlRows.Close()
	}

	query := fmt.Sprintf("SELECT * FROM %q", tableName)
	if strings.TrimSpace(whereClause) != "" {
		query += " WHERE " + whereClause
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying table %s: %w", tableName, err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("reading columns of %s: %w", tableName, err)
	}

	for rows.Next() {
		rawVals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range rawVals {
			ptrs[i] = &rawVals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("scanning row from %s: %w", tableName, err)
		}
		rowMap := make(map[string]TypedValue, len(cols))
		for i, c := range cols {
			rowMap[c] = FromSQL(rawVals[i])
		}
		slice.Rows = append(slice.Rows, rowMap)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return slice, nil
}

// ImportTableSlice ensures tableName exists in tx/db and upserts all rows from slice.
func ImportTableSlice(db *sql.DB, tx *sql.Tx, slice *TableSlice) error {
	if slice == nil {
		return nil
	}
	for _, ddl := range slice.CreateSQLs {
		safeDDL := makeCreateIfNotExists(ddl)
		if _, err := tx.Exec(safeDDL); err != nil {
			// Ignore benign DDL errors if table/index/trigger already exists.
			_ = err
		}
	}
	if len(slice.Rows) == 0 {
		return nil
	}

	destCols, err := tableColumnsTx(tx, slice.TableName)
	if err != nil {
		return fmt.Errorf("inspecting destination columns of %s: %w", slice.TableName, err)
	}
	if len(destCols) == 0 {
		return fmt.Errorf("destination table %s does not exist after DDL", slice.TableName)
	}

	for _, row := range slice.Rows {
		var cols []string
		var placeholders []string
		var vals []any

		for _, col := range destCols {
			if tv, ok := row[col]; ok {
				cols = append(cols, fmt.Sprintf("%q", col))
				placeholders = append(placeholders, "?")
				vals = append(vals, tv.ToSQL())
			}
		}
		if len(cols) == 0 {
			continue
		}
		stmt := fmt.Sprintf(
			"INSERT OR REPLACE INTO %q (%s) VALUES (%s)",
			slice.TableName,
			strings.Join(cols, ", "),
			strings.Join(placeholders, ", "),
		)
		if _, err := tx.Exec(stmt, vals...); err != nil {
			return fmt.Errorf("upserting row into %s: %w", slice.TableName, err)
		}
	}
	return nil
}

func tableColumnsTx(tx *sql.Tx, tableName string) ([]string, error) {
	rows, err := tx.Query(fmt.Sprintf("PRAGMA table_info(%q)", tableName))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}

func makeCreateIfNotExists(ddl string) string {
	replacements := []struct {
		old string
		new string
	}{
		{"CREATE TABLE ", "CREATE TABLE IF NOT EXISTS "},
		{"CREATE UNIQUE INDEX ", "CREATE UNIQUE INDEX IF NOT EXISTS "},
		{"CREATE INDEX ", "CREATE INDEX IF NOT EXISTS "},
		{"CREATE TRIGGER ", "CREATE TRIGGER IF NOT EXISTS "},
	}
	upper := strings.ToUpper(ddl)
	if strings.Contains(upper, "IF NOT EXISTS") {
		return ddl
	}
	for _, r := range replacements {
		if strings.HasPrefix(upper, r.old) {
			return r.new + ddl[len(r.old):]
		}
	}
	return ddl
}
