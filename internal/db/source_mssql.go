package db

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	_ "github.com/microsoft/go-mssqldb"
)

// MSSQLSource implements DataSource for SQL Server-managed projects.
// DSN format: mssql://user:pass@host:1433/dbname
// Version floor: SQL Server 2012+ (OFFSET/FETCH pagination).
type MSSQLSource struct {
	dsn string
}

// goDSN converts mssql://user:pass@host:port/db to the driver's sqlserver://
// URL form. Defaults: encrypt=true + trustservercertificate=true (user
// decision 2026-08-19 — encryption ON by default, opt-out per-DSN via
// ?encrypt=disable / ?trustservercertificate=false).
func (s *MSSQLSource) goDSN() (string, error) {
	u, err := url.Parse(s.dsn)
	if err != nil {
		return "", fmt.Errorf("invalid mssql dsn: %w", err)
	}
	q := u.Query()
	q.Set("database", strings.TrimPrefix(u.Path, "/"))
	if q.Get("encrypt") == "" && q.Get("trustservercertificate") == "" {
		q.Set("encrypt", "true")
		q.Set("trustservercertificate", "true")
	} else if q.Get("encrypt") == "" {
		q.Set("encrypt", "true")
	}
	// 拨号超时默认 10s — 挂死的 SQL Server 主机不得无限阻塞 handler
	// (QA 第 2 轮测试想法；0 = 驱动层"无超时"，一并强制为默认)。
	// 实测 go-mssqldb v1.10 拨号吃 "dial timeout"，"connection timeout"
	// 只限登录阶段（实测 1s 拨号超时下 connection timeout=1 仍挂 15s）。
	if dt := q.Get("dial timeout"); dt == "" || dt == "0" {
		q.Set("dial timeout", "10")
	}
	u.Scheme = "sqlserver"
	u.Path = ""
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (s *MSSQLSource) open() (*sql.DB, error) {
	dsn, err := s.goDSN()
	if err != nil {
		return nil, err
	}
	return sql.Open("sqlserver", dsn)
}

// mssqlPlaceholders builds @p1,@p2,... — go-mssqldb does NOT accept MySQL
// style "?" placeholders.
func mssqlPlaceholders(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("@p%d", i+1)
	}
	return strings.Join(parts, ",")
}

// mssqlQuoteIdent quotes a table/column identifier with square brackets.
func mssqlQuoteIdent(s string) string {
	return "[" + s + "]"
}

// mssqlSelectSQL builds a paginated SELECT. SQL Server requires ORDER BY
// before OFFSET/FETCH — (SELECT NULL) keeps result order undefined like
// other adapters' unpaginated scans.
func mssqlSelectSQL(table string, limit, offset int) string {
	if limit <= 0 {
		limit = 500
	}
	return fmt.Sprintf("SELECT * FROM [%s] ORDER BY (SELECT NULL) OFFSET %d ROWS FETCH NEXT %d ROWS ONLY",
		table, offset, limit)
}

// mssqlPkColumnSQL finds the primary key column via INFORMATION_SCHEMA.
// Placeholder is @p1 — go-mssqldb does not accept MySQL-style "?".
func mssqlPkColumnSQL() string {
	return `SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE
		WHERE TABLE_NAME=@p1 AND OBJECTPROPERTY(OBJECT_ID(CONSTRAINT_NAME),'IsPrimaryKey')=1
		ORDER BY ORDINAL_POSITION`
}

func (s *MSSQLSource) ListCollections() ([]string, error) {
	tdb, err := s.open()
	if err != nil {
		return []string{}, err
	}
	defer tdb.Close()
	rows, err := tdb.Query("SELECT TABLE_NAME FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_TYPE='BASE TABLE' ORDER BY TABLE_NAME")
	if err != nil {
		return []string{}, err
	}
	defer rows.Close()
	tables := []string{}
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			tables = append(tables, t)
		}
	}
	return tables, nil
}

func (s *MSSQLSource) ReadItems(collection string, limit, offset int) ([]map[string]interface{}, []string, string, error) {
	if err := validateTable(collection); err != nil {
		return nil, nil, "", err
	}
	tdb, err := s.open()
	if err != nil {
		return nil, nil, "", err
	}
	defer tdb.Close()
	tables := []string{collection}
	if collection == "" {
		tables = []string{"users", "user", "accounts", "member"}
	}
	for _, t := range tables {
		rows, err := tdb.Query(mssqlSelectSQL(t, limit, offset))
		if err != nil {
			continue
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		var result []map[string]interface{}
		for rows.Next() {
			vals := make([]interface{}, len(cols))
			ptrs := make([]interface{}, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			row := make(map[string]interface{})
			for i, c := range cols {
				if !IsSensitiveKey(c) {
					if b, ok := vals[i].([]byte); ok {
						row[c] = string(b)
					} else {
						row[c] = fmt.Sprintf("%v", vals[i])
					}
				}
			}
			result = append(result, row)
		}
		filteredCols := RedactSensitiveCols(cols)
		return result, filteredCols, s.pkColumn(tdb, t), nil
	}
	return []map[string]interface{}{}, []string{}, "", nil
}

func (s *MSSQLSource) CountItems(collection string) (int, error) {
	if err := validateTable(collection); err != nil {
		return 0, err
	}
	tdb, err := s.open()
	if err != nil {
		return 0, err
	}
	defer tdb.Close()
	var n int
	err = tdb.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", mssqlQuoteIdent(collection))).Scan(&n)
	return n, err
}

func (s *MSSQLSource) pkColumn(tdb *sql.DB, table string) string {
	var pk string
	tdb.QueryRow(mssqlPkColumnSQL(), table).Scan(&pk)
	return pk
}

func (s *MSSQLSource) InsertItem(collection string, data map[string]interface{}) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	tdb, err := s.open()
	if err != nil {
		return err
	}
	defer tdb.Close()
	cols := []string{}
	args := []interface{}{}
	for k, v := range data {
		cols = append(cols, mssqlQuoteIdent(k))
		args = append(args, v)
	}
	sqlStr := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", mssqlQuoteIdent(collection), strings.Join(cols, ","), mssqlPlaceholders(len(cols)))
	_, err = tdb.Exec(sqlStr, args...)
	return err
}

func (s *MSSQLSource) UpdateItem(collection, pkCol, pkVal string, data map[string]interface{}) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	tdb, err := s.open()
	if err != nil {
		return err
	}
	defer tdb.Close()
	setParts := []string{}
	args := []interface{}{}
	for k, v := range data {
		if k == pkCol {
			continue
		}
		setParts = append(setParts, fmt.Sprintf("%s=@p%d", mssqlQuoteIdent(k), len(args)+1))
		args = append(args, v)
	}
	args = append(args, pkVal)
	where := fmt.Sprintf("@p%d", len(args))
	sqlStr := fmt.Sprintf("UPDATE %s SET %s WHERE %s=%s", mssqlQuoteIdent(collection), strings.Join(setParts, ","), mssqlQuoteIdent(pkCol), where)
	_, err = tdb.Exec(sqlStr, args...)
	return err
}

func (s *MSSQLSource) DeleteItem(collection, pkCol, pkVal string) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	tdb, err := s.open()
	if err != nil {
		return err
	}
	defer tdb.Close()
	_, err = tdb.Exec(fmt.Sprintf("DELETE FROM %s WHERE %s=@p1", mssqlQuoteIdent(collection), mssqlQuoteIdent(pkCol)), pkVal)
	return err
}
