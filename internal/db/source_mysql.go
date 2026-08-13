package db

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"
)

// MySQLSource implements DataSource for MySQL-managed projects.
// DSN format: mysql://user:pass@host:3306/dbname
type MySQLSource struct {
	dsn string
}

// goDSN converts mysql://user:pass@host:port/db to the go-sql-driver format user:pass@tcp(host:port)/db.
func (s *MySQLSource) goDSN() (string, error) {
	u, err := url.Parse(s.dsn)
	if err != nil {
		return "", fmt.Errorf("invalid mysql dsn: %w", err)
	}
	pw, _ := u.User.Password()
	return fmt.Sprintf("%s:%s@tcp(%s)/%s", u.User.Username(), pw, u.Host, strings.TrimPrefix(u.Path, "/")), nil
}

func (s *MySQLSource) open() (*sql.DB, error) {
	dsn, err := s.goDSN()
	if err != nil {
		return nil, err
	}
	return sql.Open("mysql", dsn+"?timeout=5s")
}

func (s *MySQLSource) ListCollections() ([]string, error) {
	tdb, err := s.open()
	if err != nil {
		return []string{}, err
	}
	defer tdb.Close()
	rows, err := tdb.Query("SHOW TABLES")
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

func (s *MySQLSource) ReadItems(collection string) ([]map[string]interface{}, []string, string, error) {
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
		rows, err := tdb.Query(fmt.Sprintf("SELECT * FROM `%s` LIMIT 500", t))
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
		return result, filteredCols, s.pkColumn(tdb, t), nil
	}
	return []map[string]interface{}{}, []string{}, "", nil
}

func (s *MySQLSource) pkColumn(tdb *sql.DB, table string) string {
	var dbName string
	tdb.QueryRow("SELECT DATABASE()").Scan(&dbName)
	var pk string
	tdb.QueryRow(`SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND CONSTRAINT_NAME='PRIMARY' ORDER BY ORDINAL_POSITION LIMIT 1`, dbName, table).Scan(&pk)
	return pk
}

func (s *MySQLSource) InsertItem(collection string, data map[string]interface{}) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	tdb, err := s.open()
	if err != nil {
		return err
	}
	defer tdb.Close()
	cols := []string{}
	placeholders := []string{}
	args := []interface{}{}
	for k, v := range data {
		cols = append(cols, fmt.Sprintf("`%s`", k))
		placeholders = append(placeholders, "?")
		args = append(args, v)
	}
	sqlStr := fmt.Sprintf("INSERT INTO `%s` (%s) VALUES (%s)", collection, strings.Join(cols, ","), strings.Join(placeholders, ","))
	_, err = tdb.Exec(sqlStr, args...)
	return err
}

func (s *MySQLSource) UpdateItem(collection, pkCol, pkVal string, data map[string]interface{}) error {
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
		setParts = append(setParts, fmt.Sprintf("`%s`=?", k))
		args = append(args, v)
	}
	args = append(args, pkVal)
	sqlStr := fmt.Sprintf("UPDATE `%s` SET %s WHERE `%s`=?", collection, strings.Join(setParts, ","), pkCol)
	_, err = tdb.Exec(sqlStr, args...)
	return err
}

func (s *MySQLSource) DeleteItem(collection, pkCol, pkVal string) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	tdb, err := s.open()
	if err != nil {
		return err
	}
	defer tdb.Close()
	_, err = tdb.Exec(fmt.Sprintf("DELETE FROM `%s` WHERE `%s`=?", collection, pkCol), pkVal)
	return err
}
