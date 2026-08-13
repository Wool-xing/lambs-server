package db

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// SQLiteSource implements DataSource for SQLite-managed projects.
// Uses the sqlite3 CLI for both reads and writes.
type SQLiteSource struct {
	dsn string
}

// dbPath strips the sqlite:/// prefix and query string, returning the file path.
func (s *SQLiteSource) dbPath() string {
	p := strings.Replace(s.dsn, "sqlite:///", "", 1)
	if idx := strings.Index(p, "?"); idx >= 0 {
		p = p[:idx]
	}
	return p
}

func (s *SQLiteSource) ListCollections() ([]string, error) {
	cmd := exec.Command("sqlite3", s.dbPath(), "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name;")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return []string{}, err
	}
	tables := []string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			tables = append(tables, t)
		}
	}
	return tables, nil
}

func (s *SQLiteSource) ReadItems(collection string) ([]map[string]interface{}, []string, string, error) {
	if err := validateTable(collection); err != nil {
		return nil, nil, "", err
	}
	tables := []string{collection}
	if collection == "" {
		tables = []string{"users", "user", "accounts", "member"}
	}
	for _, t := range tables {
		// Get column names via PRAGMA
		cmd := exec.Command("sqlite3", s.dbPath(), fmt.Sprintf("PRAGMA table_info(%s);", t))
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			continue
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) == 0 {
			continue
		}
		var cols []string
		pk := ""
		for _, line := range lines {
			parts := strings.Split(line, "|")
			if len(parts) >= 2 {
				c := strings.TrimSpace(parts[1])
				if !strings.Contains(strings.ToLower(c), "password") && !strings.Contains(strings.ToLower(c), "token") {
					cols = append(cols, c)
				}
			}
			if len(parts) >= 6 && strings.TrimSpace(parts[5]) != "0" && pk == "" {
				pk = strings.TrimSpace(parts[1])
			}
		}
		// Read data as JSON
		quotedCols := make([]string, len(cols))
		for i, c := range cols {
			quotedCols[i] = fmt.Sprintf("\"%s\"", c)
		}
		cmd2 := exec.Command("sqlite3", "-json", s.dbPath(), fmt.Sprintf("SELECT %s FROM %s LIMIT 500;", strings.Join(quotedCols, ","), t))
		var out2 bytes.Buffer
		cmd2.Stdout = &out2
		if err := cmd2.Run(); err != nil {
			continue
		}
		var result []map[string]interface{}
		// Empty table yields empty output — treat as valid empty result
		if trimmed := strings.TrimSpace(out2.String()); trimmed != "" {
			if err := json.Unmarshal(out2.Bytes(), &result); err != nil {
				continue
			}
		}
		if result == nil {
			result = []map[string]interface{}{}
		}
		return result, cols, pk, nil
	}
	return []map[string]interface{}{}, []string{}, "", nil
}

func (s *SQLiteSource) InsertItem(collection string, data map[string]interface{}) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	colNames := []string{}
	colVals := []string{}
	for k, v := range data {
		colNames = append(colNames, fmt.Sprintf("\"%s\"", k))
		colVals = append(colVals, fmt.Sprintf("'%v'", strings.Replace(fmt.Sprintf("%v", v), "'", "''", -1)))
	}
	sqlStr := fmt.Sprintf("INSERT INTO \"%s\" (%s) VALUES (%s)", collection, strings.Join(colNames, ","), strings.Join(colVals, ","))
	cmd := exec.Command("sqlite3", s.dbPath(), sqlStr)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sqlite: %s", strings.TrimSpace(errBuf.String()))
	}
	return nil
}

func (s *SQLiteSource) UpdateItem(collection, pkCol, pkVal string, data map[string]interface{}) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	sqlStr := fmt.Sprintf("UPDATE \"%s\" SET ", collection)
	first := true
	for k, v := range data {
		if k == pkCol {
			continue
		}
		if !first {
			sqlStr += ", "
		}
		first = false
		sqlStr += fmt.Sprintf("\"%s\"='%v'", k, strings.Replace(fmt.Sprintf("%v", v), "'", "''", -1))
	}
	sqlStr += fmt.Sprintf(" WHERE \"%s\"=%s", pkCol, sqliteVal(pkVal))
	cmd := exec.Command("sqlite3", s.dbPath(), sqlStr)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sqlite: %s", strings.TrimSpace(errBuf.String()))
	}
	return nil
}

func (s *SQLiteSource) DeleteItem(collection, pkCol, pkVal string) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	cmd := exec.Command("sqlite3", s.dbPath(), fmt.Sprintf("DELETE FROM \"%s\" WHERE \"%s\"=%s", collection, pkCol, sqliteVal(pkVal)))
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sqlite: %s", strings.TrimSpace(errBuf.String()))
	}
	return nil
}
