package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// PostgresSource implements DataSource for PostgreSQL-managed projects.
type PostgresSource struct {
	dsn string
}

func (s *PostgresSource) normDSN() string {
	return strings.Replace(s.dsn, "postgresql+asyncpg://", "postgres://", 1) + "?connect_timeout=5"
}

func (s *PostgresSource) ListCollections() ([]string, error) {
	tdb, err := sql.Open("postgres", s.normDSN())
	if err != nil {
		return []string{}, err
	}
	defer tdb.Close()
	rows, err := tdb.Query("SELECT tablename FROM pg_tables WHERE schemaname NOT IN ('pg_catalog','information_schema') ORDER BY tablename")
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

func (s *PostgresSource) ReadItems(collection string) ([]map[string]interface{}, []string, string, error) {
	if err := validateTable(collection); err != nil {
		return nil, nil, "", err
	}
	tdb, err := sql.Open("postgres", s.normDSN())
	if err != nil {
		return nil, nil, "", err
	}
	defer tdb.Close()
	tables := []string{collection}
	if collection == "" {
		tables = []string{"users", "user", "accounts", "member"}
	}
	for _, t := range tables {
		rows, err := tdb.Query(fmt.Sprintf("SELECT * FROM %s LIMIT 500", t))
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
				if !strings.Contains(strings.ToLower(c), "password") && !strings.Contains(strings.ToLower(c), "token") {
					if b, ok := vals[i].([]byte); ok {
						row[c] = string(b)
					} else {
						row[c] = fmt.Sprintf("%v", vals[i])
					}
				}
			}
			result = append(result, row)
		}
		var filteredCols []string
		for _, c := range cols {
			if !strings.Contains(strings.ToLower(c), "password") && !strings.Contains(strings.ToLower(c), "token") {
				filteredCols = append(filteredCols, c)
			}
		}
		pk := s.pkColumn(tdb, t)
		return result, filteredCols, pk, nil
	}
	return []map[string]interface{}{}, []string{}, "", nil
}

func (s *PostgresSource) pkColumn(tdb *sql.DB, table string) string {
	var pk string
	tdb.QueryRow(`SELECT a.attname FROM pg_index i JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=ANY(i.indkey) WHERE i.indrelid=$1::regclass AND i.indisprimary ORDER BY array_position(i.indkey, a.attnum) LIMIT 1`, table).Scan(&pk)
	return pk
}

func (s *PostgresSource) InsertItem(collection string, data map[string]interface{}) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	tdb, err := sql.Open("postgres", s.normDSN())
	if err != nil {
		return err
	}
	defer tdb.Close()
	cols := []string{}
	placeholders := []string{}
	args := []interface{}{}
	idx := 1
	for k, v := range data {
		cols = append(cols, fmt.Sprintf("\"%s\"", k))
		placeholders = append(placeholders, fmt.Sprintf("$%d", idx))
		args = append(args, v)
		idx++
	}
	sqlStr := fmt.Sprintf("INSERT INTO \"%s\" (%s) VALUES (%s)", collection, strings.Join(cols, ","), strings.Join(placeholders, ","))
	_, err = tdb.Exec(sqlStr, args...)
	return err
}

func (s *PostgresSource) UpdateItem(collection, pkCol, pkVal string, data map[string]interface{}) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	tdb, err := sql.Open("postgres", s.normDSN())
	if err != nil {
		return err
	}
	defer tdb.Close()
	setParts := []string{}
	args := []interface{}{}
	idx := 1
	for k, v := range data {
		if k == pkCol {
			continue
		}
		setParts = append(setParts, fmt.Sprintf("\"%s\"=$%d", k, idx))
		args = append(args, v)
		idx++
	}
	args = append(args, pkVal)
	sqlStr := fmt.Sprintf("UPDATE \"%s\" SET %s WHERE \"%s\"=$%d", collection, strings.Join(setParts, ","), pkCol, idx)
	_, err = tdb.Exec(sqlStr, args...)
	return err
}

func (s *PostgresSource) DeleteItem(collection, pkCol, pkVal string) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	tdb, err := sql.Open("postgres", s.normDSN())
	if err != nil {
		return err
	}
	defer tdb.Close()
	_, err = tdb.Exec(fmt.Sprintf("DELETE FROM \"%s\" WHERE \"%s\"=$1", collection, pkCol), pkVal)
	return err
}
